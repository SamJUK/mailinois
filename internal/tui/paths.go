package tui

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// logNames are the live files a directory's rotation set is named after, in the order they're looked for.
var logNames = []string{"mail.log", "maillog"}

const journalHint = `no /var/log/mail.log or /var/log/maillog here. With the systemd journal only, try:
  journalctl -f -n all SYSLOG_FACILITY=2 -o short | mailinois inspect -`

// Expand turns the command line into the files to read, oldest first. Each argument is a file, a directory (its
// mail.log or maillog rotation set), a glob, or - for stdin. Nothing at all means /var/log.
func Expand(args []string) ([]string, error) {
	if len(args) == 0 {
		files, err := rotationSet("/var/log")
		if err != nil {
			return nil, fmt.Errorf("%s", journalHint)
		}
		return files, nil
	}
	var out []string
	for _, arg := range args {
		switch {
		case arg == "-":
			out = append(out, arg)
		case strings.ContainsAny(arg, "*?["):
			matches, err := filepath.Glob(arg)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", arg, err)
			}
			var files []string
			for _, m := range matches {
				if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
					files = append(files, m)
				}
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("%s: no files match", arg)
			}
			sortRotated(files)
			out = append(out, files...)
		default:
			fi, err := os.Stat(arg)
			if err != nil {
				return nil, err
			}
			if !fi.IsDir() {
				out = append(out, arg)
				continue
			}
			files, err := rotationSet(arg)
			if err != nil {
				return nil, err
			}
			out = append(out, files...)
		}
	}
	return out, nil
}

// rotationSet finds mail.log (or maillog) in dir and the rotated copies of it, oldest first. mail.err, mail.warn
// and mail.info are left out: Debian's rsyslog writes them as copies of lines already in mail.log.
func rotationSet(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, base := range logNames {
		var files []string
		for _, e := range entries {
			if e.Type().IsRegular() && rotationRank(base, e.Name()) != 0 {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
		if len(files) > 0 {
			sortRotated(files)
			return files, nil
		}
	}
	return nil, fmt.Errorf("%s: no %s in it; name the files instead", dir, strings.Join(logNames, " or "))
}

var (
	numbered = regexp.MustCompile(`^\.(\d+)(\.gz)?$`)     // logrotate's default: mail.log.1, mail.log.2.gz
	dated    = regexp.MustCompile(`^-(\d{8,10})(\.gz)?$`) // dateext: maillog-20261025, maillog-20261025.gz
)

// rotationRank orders name within base's rotation set, larger is newer: base itself is the newest, base.1 is
// newer than base.2, and dated copies go by date. 0 means name isn't part of the set.
func rotationRank(base, name string) int {
	if name == base {
		return math.MaxInt
	}
	rest, ok := strings.CutPrefix(name, base)
	if !ok {
		return 0
	}
	if m := numbered.FindStringSubmatch(rest); m != nil {
		n, _ := strconv.Atoi(m[1])
		return -n - 1
	}
	if m := dated.FindStringSubmatch(rest); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// sortRotated orders files oldest first within each directory and rotation set, and by name otherwise. A
// rotation set comes after a directory's other files, so its live file is the last, the one followed.
func sortRotated(files []string) {
	type key struct {
		dir, base string
		inSet     bool
		rank      int
	}
	keys := map[string]key{}
	for _, f := range files {
		dir, name := filepath.Split(f)
		k := key{dir: dir, base: name}
		for _, base := range logNames {
			if r := rotationRank(base, name); r != 0 {
				k = key{dir, base, true, r}
				break
			}
		}
		keys[f] = k
	}
	sort.SliceStable(files, func(i, j int) bool {
		a, b := keys[files[i]], keys[files[j]]
		if a.dir != b.dir {
			return a.dir < b.dir
		}
		if a.inSet != b.inSet {
			return b.inSet
		}
		if a.base != b.base {
			return a.base < b.base
		}
		return a.rank < b.rank
	})
}
