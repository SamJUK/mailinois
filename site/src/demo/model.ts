export type Status = 'sent' | 'deferred' | 'bounced' | 'rejected' | 'queued' | 'cancelled';
export type Scenario = 'throttling' | 'healthy' | 'dns';
export type Event = { at: number; kind: string; program: string; response: string; dsn: string };
export type Message = {
	id: number; queueId: string; site: string; server: string; sender: string | null;
	to: string; subject: string; provider: string; status: Status; at: number;
	size: number; delay: number; capture: boolean; events: Event[];
};
export type Filters = { site: string; server: string; period: string; q: string };

export const sites = [
	{ id: 'storefront', name: 'Storefront', domain: 'storefront.example', from: 'orders', subject: 'Your order is confirmed' },
	{ id: 'accounts', name: 'Accounts', domain: 'accounts.example', from: 'hello', subject: 'Welcome to your account' },
	{ id: 'newsletter', name: 'Newsletter', domain: 'letters.example', from: 'news', subject: 'This week at the workshop' },
];
export const servers = [
	{ name: 'relay-1', ip: '203.0.113.10' }, { name: 'relay-2', ip: '198.51.100.20' },
];
export const providers = ['google', 'microsoft', 'yahoo', 'apple', 'other'];
export const providerName = (name: string) => ({ google: 'Google', microsoft: 'Microsoft', yahoo: 'Yahoo', apple: 'Apple', other: 'Other' }[name] ?? name);
export const periods: Record<string, number> = { '1h': 3600000, '24h': 86400000, '7d': 604800000, '30d': 2592000000 };
export const pending = (m: Message) => m.status === 'deferred' || m.status === 'queued';
export const accepted = (m: Message) => m.status !== 'rejected' && m.status !== 'cancelled';

const replies: Record<Status, [string, string]> = {
	sent: ['250 2.0.0 Message accepted for delivery', '2.0.0'],
	deferred: ['451 4.7.650 Temporarily rate limited by the receiving server. Try again later.', '4.7.650'],
	bounced: ['550 5.1.1 The recipient mailbox does not exist', '5.1.1'],
	rejected: ['554 5.7.1 Relay access denied', '5.7.1'],
	queued: ['Waiting for the first delivery attempt', ''],
	cancelled: ['SMTP transaction cancelled before message acceptance', ''],
};

function randomSource(seed: number) {
	return () => { seed = Math.imul(1664525, seed) + 1013904223 | 0; return (seed >>> 0) / 4294967296; };
}

function makeMessage(id: number, at: number, now: number, scenario: Scenario, random: () => number, fixed: Partial<Message> = {}): Message {
	const site = sites[Math.floor(random() * sites.length)];
	const provider = providers[Math.floor(random() * providers.length)];
	const roll = random();
	let status: Status = roll < .91 ? 'sent' : roll < .96 ? 'bounced' : 'rejected';
	if (scenario === 'throttling' && provider === 'microsoft' && now - at < 4 * periods['1h']) status = 'deferred';
	const m: Message = {
		id, queueId: (0x4a00000000 + id * 7919).toString(16).toUpperCase(),
		site: site.id, server: servers[id % servers.length].name, sender: `${site.from}@${site.domain}`,
		to: `customer${id}@${provider === 'other' ? 'customer' : provider}.example`,
		subject: `${site.subject}${site.id === 'storefront' ? ` #${1000 + id}` : ''}`,
		provider, status, at, size: 2000 + Math.floor(random() * 20000),
		delay: 1 + Math.floor(random() * 25), capture: random() > .2, events: [], ...fixed,
	};
	if (m.status === 'rejected') { m.queueId = ''; m.capture = false; }
	if (m.status === 'cancelled') { m.sender = null; m.to = ''; m.subject = ''; m.capture = false; }
	const event = (seconds: number, kind: string, program: string, response: string, dsn = '') => ({ at: at + seconds * 1000, kind, program, response, dsn });
	m.events.push(event(0, 'connection', 'postfix/smtpd', `client=app-${m.site}[10.0.0.20]`));
	if (m.status === 'cancelled') m.events.push(event(1, 'cancelled', 'postfix/cleanup', 'removed (canceled)'));
	else if (m.status === 'rejected') m.events.push(event(1, 'rejected', 'postfix/smtpd', replies.rejected[0], replies.rejected[1]));
	else {
		m.events.push(event(.1, 'accepted', 'postfix/qmgr', `from=<${m.sender}>, size=${m.size}, nrcpt=1 (queue active)`));
		if (m.status === 'deferred') {
			for (const seconds of [1, 120, 600]) {
				if (at + seconds * 1000 <= now) m.events.push(event(seconds, 'deferred', 'postfix/smtp', replies.deferred[0], replies.deferred[1]));
			}
		} else if (m.status !== 'queued') {
			m.delay = Math.min(m.delay, Math.max(.2, (now - at) / 1000 - 1));
			if (m.status === 'sent' && scenario !== 'healthy' && id % 17 === 0 && now - at > 700000) {
				m.events.push(event(1, 'deferred', 'postfix/smtp', replies.deferred[0], replies.deferred[1]));
				m.delay = 680;
			}
			m.events.push(event(m.delay, m.status, 'postfix/smtp', replies[m.status][0], replies[m.status][1]));
			m.events.push(event(m.delay + .1, 'removed', 'postfix/qmgr', 'removed'));
		}
	}
	return m;
}

export function createDemo(now = Date.now(), scenario: Scenario = 'throttling'): Message[] {
	const random = randomSource(20261019);
	const messages: Message[] = [];
	for (let id = 7; id <= 1260; id++) {
		const age = id < 180 ? 30000 + random() * (periods['24h'] - 30000) : periods['24h'] + random() * 59 * periods['24h'];
		messages.push(makeMessage(id, now - age, now, scenario, random));
	}
	const special: Partial<Message>[] = [
		{ site: 'storefront', sender: 'orders@storefront.example', provider: 'microsoft', to: 'alex@microsoft.example', subject: 'Your order is confirmed #1042', status: scenario === 'throttling' ? 'deferred' : 'sent', capture: true },
		{ site: 'accounts', sender: 'hello@accounts.example', provider: 'google', to: 'jules@google.example', subject: 'Welcome to your account', status: 'sent', capture: true },
		{ site: 'newsletter', sender: 'news@letters.example', provider: 'yahoo', to: 'old-address@yahoo.example', status: 'bounced', subject: 'This week at the workshop' },
		{ site: 'accounts', sender: 'hello@accounts.example', status: 'rejected', subject: '' },
		{ site: 'storefront', sender: 'orders@storefront.example', status: 'queued', subject: 'Your order is confirmed #1043', capture: true },
		{ site: 'accounts', status: 'cancelled' },
	];
	const ages = [720, 480, 360, 240, 8, 60];
	special.forEach((fixed, i) => messages.push(makeMessage(i + 1, now - ages[i] * 1000, now, scenario, random, fixed)));
	return messages.sort((a, b) => b.at - a.at);
}

function searchTime(value: string, now: number) {
	const relative = /^(\d+)([mhd])$/.exec(value);
	return relative ? now - Number(relative[1]) * ({ m: 60000, h: periods['1h'], d: periods['24h'] }[relative[2]] ?? 0) : Date.parse(value);
}

export function filterMessages(messages: Message[], filters: Filters, now: number, queue = false) {
	const terms = [...filters.q.matchAll(/(-?)(?:([a-z]+):)?(?:"([^"]*)"|(\S+))/gi)].map((m) => ({ not: !!m[1], field: (m[2] ?? '').toLowerCase(), value: (m[3] ?? m[4]).toLowerCase() }));
	const after = terms.find((t) => !t.not && t.field === 'after');
	const since = after ? searchTime(after.value, now) : now - (periods[filters.period] ?? periods['24h']);
	return messages.filter((m) => {
		if (filters.site && m.site !== filters.site || filters.server && m.server !== filters.server) return false;
		if (queue ? !pending(m) : m.at < since) return false;
		return terms.every(({ not, field, value }) => {
			let match: boolean;
			switch (field) {
				case 'status': match = value.split(',').includes(m.status); break;
				case 'from': match = (m.sender ?? '').toLowerCase().includes(value); break;
				case 'to': match = m.to.toLowerCase().includes(value); break;
				case 'domain': match = `${m.sender} ${m.to}`.toLowerCase().includes(value); break;
				case 'provider': match = m.provider === value; break;
				case 'server': match = m.server.toLowerCase().includes(value); break;
				case 'site': match = m.site === value; break;
				case 'qid': match = m.queueId.toLowerCase() === value; break;
				case 'subject': match = m.subject.toLowerCase().includes(value); break;
				case 'dsn': match = m.events.some((e) => e.dsn.startsWith(value)); break;
				case 'has': match = value === 'capture' && m.capture; break;
				case 'after': match = m.at >= searchTime(value, now); break;
				case 'before': match = m.at < searchTime(value, now); break;
				default: match = `${m.sender} ${m.to} ${m.subject} ${m.queueId}`.toLowerCase().includes(field ? `${field}:${value}` : value);
			}
			return not ? !match : match;
		});
	}).sort((a, b) => b.at - a.at);
}

export function totals(messages: Message[]) {
	const finals = messages.filter((m) => m.status === 'sent' || m.status === 'bounced');
	const delays = messages.filter((m) => m.status === 'sent').map((m) => m.delay).sort((a, b) => a - b);
	return {
		received: messages.filter(accepted).length,
		sent: messages.filter((m) => m.status === 'sent').length,
		bounced: messages.filter((m) => m.status === 'bounced').length,
		rejected: messages.filter((m) => m.status === 'rejected').length,
		deferrals: messages.flatMap((m) => m.events).filter((e) => e.kind === 'deferred').length,
		queue: messages.filter(pending).length,
		delivered: finals.length ? 100 * messages.filter((m) => m.status === 'sent').length / finals.length : null,
		median: delays.length ? delays[Math.floor(delays.length / 2)] : null,
		p95: delays.length ? delays[Math.min(delays.length - 1, Math.floor(delays.length * .95))] : null,
	};
}

export function advance(messages: Message[], now: number, scenario: Scenario) {
	for (const m of messages.filter((m) => m.status === 'queued' && now - m.at > 4000)) {
		m.status = scenario === 'throttling' && m.provider === 'microsoft' ? 'deferred' : 'sent';
		m.delay = (now - m.at) / 1000;
		m.events.push({ at: now, kind: m.status, program: 'postfix/smtp', response: replies[m.status][0], dsn: replies[m.status][1] });
		if (m.status === 'sent') m.events.push({ at: now, kind: 'removed', program: 'postfix/qmgr', response: 'removed', dsn: '' });
	}
	const id = Math.max(...messages.map((m) => m.id)) + 1;
	messages.unshift(makeMessage(id, now - 100, now, scenario, randomSource(id), { status: 'queued' }));
}
