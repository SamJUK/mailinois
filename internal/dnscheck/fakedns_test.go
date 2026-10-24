package dnscheck

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
)

// fakeDNS answers the real Go resolver from records held in the test. Each lookup dials a net.Pipe
// instead of a socket; as that isn't a PacketConn, the resolver uses TCP framing (a two-byte length
// before each message), so no port is opened and no packet leaves the process.
type fakeDNS struct {
	records  []rr
	servfail map[string]bool // names answered with SERVFAIL
}

type rr struct {
	name  string
	rtype uint16
	data  []byte
}

const (
	typeA    = 1
	typeTXT  = 16
	typeAAAA = 28
)

// txt is one TXT record holding each of strs as a separate character-string. Like zone file tools, it
// splits a string longer than 255 bytes into several.
func txt(name string, strs ...string) rr {
	var data []byte
	for _, s := range strs {
		for len(s) > 255 {
			data = append(append(data, 255), s[:255]...)
			s = s[255:]
		}
		data = append(append(data, byte(len(s))), s...)
	}
	return rr{name, typeTXT, data}
}

func addr(name, ip string) rr {
	a := net.ParseIP(ip)
	if v4 := a.To4(); v4 != nil {
		return rr{name, typeA, v4}
	}
	return rr{name, typeAAAA, a.To16()}
}

func (f *fakeDNS) resolver() *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go f.serve(server)
		return client, nil
	}}
}

func (f *fakeDNS) serve(conn net.Conn) {
	defer conn.Close()
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return
	}
	query := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, query); err != nil {
		return
	}
	resp := f.answer(query)
	conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(resp))), resp...))
}

// answer builds the response to a query: the header, the question echoed back, and the matching records.
// A name with no records at all is NXDOMAIN; a known name without the type asked for is an empty answer.
func (f *fakeDNS) answer(query []byte) []byte {
	name, end := questionName(query)
	qtype := binary.BigEndian.Uint16(query[end:])

	rcode := uint16(3) // NXDOMAIN
	var answers []rr
	if f.servfail[name] {
		rcode = 2
	} else {
		for _, r := range f.records {
			if strings.EqualFold(r.name, name) {
				rcode = 0
				if r.rtype == qtype {
					answers = append(answers, r)
				}
			}
		}
	}

	msg := append([]byte(nil), query[:2]...)               // ID
	msg = binary.BigEndian.AppendUint16(msg, 0x8180|rcode) // response, recursion desired and available
	msg = binary.BigEndian.AppendUint16(msg, 1)            // one question
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(answers)))
	msg = append(msg, 0, 0, 0, 0) // no authority or additional records
	msg = append(msg, query[12:end+4]...)
	for _, r := range answers {
		msg = append(msg, 0xc0, 12) // the name: a pointer to the question's
		msg = binary.BigEndian.AppendUint16(msg, r.rtype)
		msg = binary.BigEndian.AppendUint16(msg, 1)  // class IN
		msg = binary.BigEndian.AppendUint32(msg, 60) // TTL
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(r.data)))
		msg = append(msg, r.data...)
	}
	return msg
}

// questionName reads the query's (uncompressed) name, returning it without the trailing dot and the
// offset just past it.
func questionName(query []byte) (string, int) {
	var labels []string
	off := 12
	for query[off] != 0 {
		n := int(query[off])
		labels = append(labels, string(query[off+1:off+1+n]))
		off += 1 + n
	}
	return strings.ToLower(strings.Join(labels, ".")), off + 1
}
