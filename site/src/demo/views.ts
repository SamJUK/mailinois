import { accepted, filterMessages, pending, periods, providerName, providers, servers, sites, totals, type Event, type Filters, type Message, type Scenario } from './model.ts';

export type Route = Filters & { view: string; id: number; tab: string; page: number; domain: string };
export type Context = { route: Route; messages: Message[]; now: number; scenario: Scenario };
export const labels: Record<string, string> = { overview: 'Overview', messages: 'Messages', queue: 'Queue', providers: 'Providers', domains: 'Domains', servers: 'Servers', alerts: 'Alerts' };
export const escape = (value: unknown) => String(value ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]!));
const e = escape;
const number = (n: number) => n.toLocaleString('en-GB');
const percent = (n: number | null) => n === null ? '–' : `${n.toFixed(1)}%`;
const when = (at: number) => new Date(at).toLocaleString('en-GB', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });
const clock = (at: number) => new Date(at).toLocaleTimeString('en-GB', { hour12: false });
const age = (at: number, now: number) => { const s = Math.max(0, Math.floor((now - at) / 1000)); return s < 60 ? `${s}s` : s < 3600 ? `${Math.floor(s / 60)}m` : s < 86400 ? `${Math.floor(s / 3600)}h` : `${Math.floor(s / 86400)}d`; };
const seconds = (value: number | null) => value === null ? '–' : value < 60 ? `${value.toFixed(1)}s` : `${(value / 60).toFixed(1)}m`;
const badge = (status: string) => `<span class="badge s-${e(({ pass: 'sent', fail: 'bounced', ok: 'sent', firing: 'bounced' } as Record<string, string>)[status] ?? status)}">${e(status)}</span>`;
const sender = (m: Message) => m.sender === null ? '(sender not logged)' : m.sender || '<> (bounce)';
const lastAttempt = (m: Message) => m.events.findLast((ev) => ['sent', 'deferred', 'bounced', 'rejected'].includes(ev.kind));
const attempts = (m: Message) => m.events.filter((ev) => ['sent', 'deferred', 'bounced'].includes(ev.kind)).length;

export function readRoute(hash: string): Route {
	const [path, query = ''] = hash.replace(/^#\/?/, '').split('?');
	const params = new URLSearchParams(query);
	const [view = '', id = ''] = path.split('/');
	return {
		view: view || 'overview', id: /^\d+$/.test(id) ? Number(id) : 0,
		tab: ['timeline', 'headers', 'body', 'log'].includes(params.get('tab') ?? '') ? params.get('tab')! : 'timeline',
		site: sites.some((s) => s.id === params.get('site')) ? params.get('site')! : '',
		server: servers.some((s) => s.name === params.get('server')) ? params.get('server')! : '',
		period: Object.hasOwn(periods, params.get('period') ?? '') ? params.get('period')! : '24h',
		q: (params.get('q') ?? '').slice(0, 500), page: Math.min(1000, Math.max(0, Number(params.get('page')) || 0)) | 0,
		domain: sites.some((s) => s.domain === params.get('domain')) ? params.get('domain')! : '',
	};
}

export function link(route: Route, view = route.view, changes: Record<string, string | number> = {}) {
	const params = new URLSearchParams();
	for (const [key, value] of Object.entries({ site: route.site, server: route.server, period: route.period, ...changes })) {
		if (value !== '' && !(key === 'period' && value === '24h')) params.set(key, String(value));
	}
	const path = view === 'overview' ? '' : view;
	return `#/${path}${params.size ? '?' + params : ''}`;
}

const anchor = (c: Context, view: string, text: string, changes: Record<string, string | number> = {}, className = '') => `<a${className ? ` class="${e(className)}"` : ''} href="${e(link(c.route, view, changes))}">${text}</a>`;
const messageLink = (c: Context, m: Message, text = e(m.queueId || 'NOQUEUE')) => anchor(c, `messages/${m.id}`, text);
const empty = (text: string) => `<p class="empty">${text}</p>`;
function table(headings: string[], rows: string[][], noRows = 'Nothing to show in this period.') {
	return rows.length ? `<div class="scroll"><table><thead><tr>${headings.map((h) => `<th>${h}</th>`).join('')}</tr></thead><tbody>${rows.map((row) => `<tr>${row.map((cell) => `<td>${cell}</td>`).join('')}</tr>`).join('')}</tbody></table></div>` : empty(noRows);
}
const panel = (title: string, content: string, aside = '', flush = true) => `<section class="panel${flush ? ' flush' : ''}" aria-label="${e(title.replace(/<[^>]*>/g, ''))}"><div class="panel-head"><h2>${title}</h2>${aside}</div>${content}</section>`;
const selected = (c: Context, queue = false, q = '') => filterMessages(c.messages, { ...c.route, q }, c.now, queue);

function failureTable(c: Context, messages: Message[]) {
	return table(['When', 'Server', 'Recipient', 'Status', 'Response'], messages.filter((m) => ['deferred', 'bounced', 'rejected'].includes(m.status)).sort((a, b) => (lastAttempt(b)?.at ?? b.at) - (lastAttempt(a)?.at ?? a.at)).slice(0, 10).map((m) => [messageLink(c, m, e(when(lastAttempt(m)?.at ?? m.at))), e(m.server), e(m.to), badge(m.status), `<span class="sub">${e(lastAttempt(m)?.response)}</span>`]), 'Nothing failed in this period.');
}

function reasons(c: Context, messages: Message[]) {
	const groups = providers.map((provider) => {
		const events = messages.filter((m) => m.provider === provider).flatMap((m) => m.events).filter((ev) => ev.kind === 'deferred');
		return { provider, events };
	}).filter((g) => g.events.length);
	return table(['Provider', 'Code', 'Deferrals', 'Latest response'], groups.map(({ provider, events }) => [anchor(c, 'messages', e(providerName(provider)), { q: `provider:${provider} dsn:4.7` }), '<code>4.7.650</code>', number(events.length), `<span class="sub">${e(events[events.length - 1].response)}</span>`]), 'No deferrals in this period.');
}

function chart(c: Context, messages: Message[]) {
	const span = periods[c.route.period];
	const buckets = Array.from({ length: 24 }, () => ({ sent: 0, deferred: 0, bounced: 0, rejected: 0 }));
	for (const m of messages) for (const ev of m.events) {
		const index = Math.floor((ev.at - (c.now - span)) / span * buckets.length);
		if (index >= 0 && index < 24 && Object.hasOwn(buckets[index], ev.kind)) buckets[index][ev.kind as keyof typeof buckets[0]]++;
	}
	const max = Math.max(1, ...buckets.map((b) => Object.values(b).reduce((a, v) => a + v, 0)));
	const bars = buckets.map((bucket, i) => {
		let y = 180;
		const rectangles = Object.entries(bucket).map(([kind, count]) => { const height = count / max * 170; y -= height; return `<rect class="${kind}" x="${i * 30 + 3}" y="${y}" width="23" height="${height}"/>`; }).join('');
		const at = c.now - span + i / 24 * span;
		return `<g><title>${e(when(at))}: ${Object.entries(bucket).map(([kind, count]) => `${count} ${kind}`).join(', ')}</title><rect class="hit" x="${i * 30}" y="0" width="30" height="180"/>${rectangles}</g>${i % 6 === 0 ? `<text x="${i * 30 + 15}" y="202">${e(c.route.period === '1h' || c.route.period === '24h' ? new Date(at).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' }) : new Date(at).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' }))}</text>` : ''}`;
	}).join('');
	return `<svg class="chart" viewBox="0 -16 720 230" role="img" aria-label="Simulated delivery outcomes over the selected period"><line class="axis" x1="0" x2="720" y1="180" y2="180"/><line class="grid" x1="0" x2="720" y1="10" y2="10"/><text class="max" x="0" y="0">${max}</text>${bars}</svg>`;
}

export function tail(c: Context) {
	return selected(c).sort((a, b) => b.events[b.events.length - 1].at - a.events[a.events.length - 1].at).slice(0, 9).map((m) => `<li><span class="mono when">${e(clock(m.events[m.events.length - 1].at))}</span><i class="dot s-${e(m.status)}"></i>${messageLink(c, m, `<span class="to">${e(m.to || '(no recipient)')}</span><span class="mono what">${e(m.server)} · ${e(m.status)}</span>`)}</li>`).join('');
}

export function overview(c: Context) {
	const rows = selected(c), queue = selected(c, true), t = totals(rows);
	const metric = (name: string, value: string, note: string, to: string, q = '') => anchor(c, to, `<span class="label">${name}</span><strong>${value}</strong><em>${note}</em>`, { q });
	const kpis = `<section class="kpis panel" aria-label="Totals">${metric('Received', number(t.received), 'messages accepted', 'messages')}${metric('Delivered', percent(t.delivered), `${number(t.sent)} recipients`, 'messages', 'status:sent')}${metric('Deferrals', number(t.deferrals), 'retried by Postfix', 'messages', 'dsn:4.7')}${metric('Bounced', number(t.bounced), 'recipient failures', 'messages', 'status:bounced')}${metric('Rejected', number(t.rejected), 'refused at SMTP', 'messages', 'status:rejected')}${metric('In queue', number(queue.length), queue.length ? `oldest ${age(Math.min(...queue.map((m) => m.at)), c.now)}` : 'empty', 'queue')}<div><span class="label">Delivery time</span><strong>${seconds(t.median)}</strong><em>median · p95 ${seconds(t.p95)}</em></div></section>`;
	const legend = '<ul class="legend"><li class="sent">sent</li><li class="deferred">deferred</li><li class="bounced">bounced</li><li class="rejected">rejected</li></ul>';
	const domains = sites.filter((s) => !c.route.site || c.route.site === s.id).map((s) => `<div class="domline">${anchor(c, 'domains', e(s.domain), { domain: s.domain })}${badge(c.scenario === 'dns' && s.id === 'newsletter' ? 'fail' : 'pass')}</div>`).join('');
	return `${kpis}<div class="split"><div class="split-main">${panel('Outcomes', chart(c, rows), legend, false)}${panel('Recent failures', failureTable(c, rows), anchor(c, 'queue', 'Queue and failures →'))}${panel('Why mail was deferred', reasons(c, rows), anchor(c, 'messages', 'All attempts →', { q: 'dsn:4.7' }))}</div><aside class="split-side">${panel('<i class="pulse"></i>Simulated live tail', `<ol class="tail" id="demo-tail">${tail(c)}</ol>`)}${panel('Blocklists', servers.filter((s) => !c.route.server || s.name === c.route.server).map((s) => `<div class="ipline"><span class="mono">${s.ip}<small>${s.name}.mail.example</small></span><span class="zonebar">${['Barracuda', 'SpamCop', 'PSBL', 'Mailspike'].map((name) => `<i class="z-clean" title="${name}: simulated clean result"></i>`).join('')}</span></div>`).join(''), anchor(c, 'servers', 'Servers →'), false)}${panel('Sending domains', domains, anchor(c, 'domains', 'Domains →'), false)}</aside></div>`;
}

export function messages(c: Context) {
	const rows = filterMessages(c.messages, c.route, c.now);
	const page = Math.min(c.route.page, Math.max(0, Math.ceil(rows.length / 30) - 1));
	const chips = ['', 'sent', 'deferred', 'bounced', 'rejected', 'queued', 'cancelled'].map((s) => `<a href="${e(link(c.route, 'messages', { q: s ? `status:${s}` : '' }))}"${c.route.q === (s ? `status:${s}` : '') ? ' aria-current="true"' : ''}>${s || 'All'}</a>`).join('');
	return `<div class="toolbar"><div class="chips">${chips}</div><details class="help"><summary>Search tips</summary><div><p>Search addresses, subjects or queue IDs. Add fields to narrow the results; prefix a field with <code>-</code> to leave it out.</p><p><code>provider:microsoft status:deferred</code><br><code>from:storefront.example has:capture</code><br><code>subject:"Your order" after:7d</code><br><code>dsn:4.7 -status:sent</code></p><p>Also: <code>to:</code>, <code>domain:</code>, <code>site:</code>, <code>server:</code>, <code>qid:</code>, <code>before:</code>.</p></div></details></div><p class="lede" role="status">${number(rows.length)} matching messages${c.route.q ? ` for <code>${e(c.route.q)}</code>` : ''}</p><section class="panel flush">${table(['Received', 'Server', 'From', 'To', 'Subject', 'Status', 'Size', 'Queue ID'], rows.slice(page * 30, page * 30 + 30).map((m) => [messageLink(c, m, e(when(m.at))), e(m.server), `<span class="demo-address">${e(sender(m))}</span>`, `<span class="demo-address">${e(m.to || '–')}</span>`, messageLink(c, m, e(m.subject || '(no subject logged)')), badge(m.status), `${(m.size / 1024).toFixed(1)} KB`, `<span class="mono">${messageLink(c, m)}</span>`]), 'No messages match. Try a longer period, another site or fewer search terms.')}</section>${rows.length > 30 ? `<nav class="pager" aria-label="Message pages">${page > 0 ? anchor(c, 'messages', '← Newer', { q: c.route.q, page: page - 1 }) : '<span></span>'}<span class="sub">Page ${page + 1} of ${Math.ceil(rows.length / 30)}</span>${(page + 1) * 30 < rows.length ? anchor(c, 'messages', 'Older →', { q: c.route.q, page: page + 1 }) : '<span></span>'}</nav>` : ''}`;
}

export function queue(c: Context) {
	const rows = selected(c, true).sort((a, b) => a.at - b.at);
	return panel('In the queue now', table(['Waiting', 'Server', 'Queue ID', 'From', 'Attempts', 'Last response'], rows.map((m) => [age(m.at, c.now), e(m.server), `<span class="mono">${messageLink(c, m)}</span>`, e(sender(m)), String(attempts(m)), e(lastAttempt(m)?.response ?? 'Waiting for the first delivery attempt')]), 'The queue is empty.'), '<span class="sub">current queue, across all periods</span>') + panel('Why mail was deferred', reasons(c, selected(c))) + panel('Failures', failureTable(c, selected(c)));
}

export function providerView(c: Context) {
	const rows = selected(c);
	return `<p class="lede">Simulated attempts in the selected period. Choose a provider to inspect its messages.</p><div class="providers">${providers.map((provider) => {
		const matches = rows.filter((m) => m.provider === provider), t = totals(matches);
		const events = matches.flatMap((m) => m.events).filter((ev) => ['sent', 'deferred', 'bounced'].includes(ev.kind));
		const responses = [...new Map(events.filter((ev) => ev.kind !== 'sent').map((ev) => [ev.dsn, ev])).values()];
		return panel(anchor(c, 'messages', e(providerName(provider)), { q: `provider:${provider}` }), `<div class="rates"><div><span class="label">Delivered</span><strong>${percent(t.delivered)}</strong></div><div><span class="label">Deferred</span><strong>${percent(events.length ? t.deferrals / events.length * 100 : null)}</strong></div><div><span class="label">Bounced</span><strong>${number(t.bounced)}</strong></div></div><h3>Responses</h3>${responses.length ? `<ul class="responses">${responses.map((ev) => `<li>${badge(ev.kind)} <code>${e(ev.dsn)}</code><div class="response">${e(ev.response)}</div></li>`).join('')}</ul>` : empty(events.length ? 'Every delivery attempt succeeded.' : 'No delivery attempts in this period.')}`, `<span class="sub">${number(events.length)} attempts</span>`, false);
	}).join('')}</div>`;
}

export function domainView(c: Context) {
	const visible = sites.filter((s) => !c.route.site || c.route.site === s.id);
	return `<p class="lede">Simulated DNS results. Expand a domain for the sending IPs, selectors and records.</p><section class="panel flush domain-list" aria-label="Domain health"><div class="domain-heading" aria-hidden="true"><span>Domain</span><span>SPF</span><span>DKIM</span><span>DMARC</span><span>Overall</span><span>Checked</span></div>${visible.map((site) => {
		const broken = c.scenario === 'dns' && site.id === 'newsletter';
		const checks = [
			...servers.filter((s) => !c.route.server || c.route.server === s.name).map((s) => ({ kind: 'SPF', subject: s.ip, status: 'pass', summary: 'The sending IP is authorised.', record: `v=spf1 ip4:${servers[0].ip} ip4:${servers[1].ip} -all` })),
			{ kind: 'DKIM', subject: 'demo2026', status: broken ? 'fail' : 'pass', summary: broken ? 'No DKIM record found for selector demo2026.' : 'The public key is published for the signing selector.', record: broken ? '(no record)' : 'v=DKIM1; k=rsa; p=(simulated public key)' },
			{ kind: 'DMARC', subject: site.domain, status: 'pass', summary: 'A reject policy is published.', record: `v=DMARC1; p=reject; rua=mailto:reports@${site.domain}` },
		];
		return `<details class="domain-row"${c.route.domain === site.domain ? ' open' : ''}><summary><span class="domain-name">${site.domain}</span>${['SPF', 'DKIM', 'DMARC'].map((kind) => `<span class="domain-check"><span class="check-label">${kind} </span>${badge(kind === 'DKIM' && broken ? 'fail' : 'pass')}</span>`).join('')}<span class="domain-overall"><span class="check-label">Overall </span>${badge(broken ? 'fail' : 'pass')}</span><span class="domain-checked">just now</span></summary><div class="domain-details">${table(['Check', 'IP or selector', 'Result', 'What it means', 'Simulated record'], checks.map((check) => [check.kind, e(check.subject), badge(check.status), e(check.summary), `<code class="record">${e(check.record)}</code>`]))}</div></details>`;
	}).join('')}</section>`;
}

export function serverView(c: Context) {
	const scope = filterMessages(c.messages, { ...c.route, period: '24h', q: '' }, c.now);
	return panel('Postfix servers', table(['Server', 'Last line', 'Received', 'Delivered', 'Deferrals', 'Failed', 'In queue'], servers.filter((s) => !c.route.server || s.name === c.route.server).map((server) => {
		const rows = scope.filter((m) => m.server === server.name), t = totals(rows);
		return [anchor(c, 'overview', server.name, { server: server.name }), rows.length ? `${age(Math.max(...rows.map((m) => m.at)), c.now)} ago` : 'no traffic', number(t.received), number(t.sent), number(t.deferrals), number(t.bounced + t.rejected), number(t.queue)];
	})), '<span class="sub">last 24 hours</span>') + panel('Sending IPs', `<div class="ips">${servers.filter((s) => !c.route.server || c.route.server === s.name).map((s) => `<div class="ip"><div class="ip-head"><strong class="mono">${s.ip}</strong><span>${s.name}.mail.example</span>${badge('pass')}<span class="sub">simulated forward-confirmed PTR</span></div><ul class="zones">${['Barracuda', 'SpamCop', 'PSBL', 'Mailspike'].map((name) => `<li class="z-clean">${name}: clean</li>`).join('')}</ul></div>`).join('')}</div>`, '<span class="sub">reserved example IP addresses</span>', false);
}

export function alertView(c: Context) {
	const microsoft = selected(c).filter((m) => m.provider === 'microsoft');
	const events = microsoft.flatMap((m) => m.events).filter((ev) => ['sent', 'deferred', 'bounced'].includes(ev.kind));
	const rate = events.length ? events.filter((ev) => ev.kind === 'deferred').length / events.length * 100 : 0;
	const rules: { name: string; value: string; above: string; firing: boolean; target: string; query: Record<string, string>; scope: string }[] = [
		{ name: 'Microsoft deferral rate', value: percent(rate), above: '10%', firing: rate > 10, target: 'messages', query: { q: 'provider:microsoft dsn:4.7' }, scope: 'Microsoft in this view' },
		{ name: 'Queue depth', value: String(selected(c, true).length), above: '20 messages', firing: selected(c, true).length > 20, target: 'queue', query: {}, scope: 'current site and server' },
		{ name: 'Sending domain DNS', value: c.scenario === 'dns' && (!c.route.site || c.route.site === 'newsletter') ? '1 failure' : '0 failures', above: '0 failures', firing: c.scenario === 'dns' && (!c.route.site || c.route.site === 'newsletter'), target: 'domains', query: { domain: 'letters.example' }, scope: 'configured domains' },
	];
	return panel('Rules', table(['Alert', 'State', 'Now', 'Above', 'Scope', 'Notifies'], rules.map((rule) => [anchor(c, rule.target, `<strong>${rule.name}</strong>`, rule.query), badge(rule.firing ? 'firing' : 'ok'), rule.value, rule.above, rule.scope, '<span class="sub">Demo only</span>']))) + panel('About these alerts', '<p class="lede">These rules are evaluated against the simulated data in this view. Try the Broken DNS scenario to see a domain alert, or Healthy traffic to clear the Microsoft delay. No notifications are sent.</p>', '', false);
}

export function messageBody(m: Message) {
	return `Hello,\n\n${m.subject}.\n\nThis is a made-up message in the Mailinois browser demo.\nNo email has been sent.\n\nThanks,\nThe ${sites.find((s) => s.id === m.site)?.name ?? 'demo'} team`;
}

export function messageHeaders(m: Message): [string, string][] {
	return [['From', sender(m)], ['To', m.to], ['Subject', m.subject], ['Date', new Date(m.at).toUTCString()], ['Message-ID', `<demo-${m.id}@${sites.find((s) => s.id === m.site)!.domain}>`], ['Received', `from app-${m.site} by ${m.server}.mail.example with ESMTP id ${m.queueId}`], ['DKIM-Signature', 'v=1; a=rsa-sha256; s=demo2026; b=(simulated signature)'], ['MIME-Version', '1.0'], ['Content-Type', 'text/plain; charset=utf-8']];
}

export function messageView(c: Context) {
	const m = c.messages.find((m) => m.id === c.route.id && (!c.route.site || m.site === c.route.site) && (!c.route.server || m.server === c.route.server));
	if (!m) return panel('Message not found', empty('This message is outside the selected site or server, or the demo was reset.') + anchor(c, 'messages', 'Back to messages'), '', false);
	const attempt = lastAttempt(m);
	const explanation = m.status === 'cancelled' ? 'The SMTP transaction ended before acceptance. It is outside the queue and Received count.' : m.status === 'queued' ? 'Postfix is waiting to make its first delivery attempt.' : m.status === 'rejected' ? 'Refused during the SMTP conversation, before being queued.' : attempt?.response ?? '';
	const facts = [['From', e(sender(m))], ['Handed over by', `app-${e(m.site)} [10.0.0.20]`], ['Server', e(m.server)], ['Received', e(when(m.at))], ['Queue ID', e(m.queueId || 'none: refused before queueing')], ['Left the queue', pending(m) ? 'still queued' : e(when(m.events[m.events.length - 1].at))], ['Size', `${(m.size / 1024).toFixed(1)} KB`], ['Capture', m.capture ? 'full (simulated)' : 'not captured']];
	const header = `<p class="crumbs">${anchor(c, 'messages', 'Messages')} › <span class="mono">${e(m.queueId || 'NOQUEUE')}</span></p><header class="msghead"><div><h1>${e(m.subject || '(no subject logged)')}</h1><p>${badge(m.status)} ${e(explanation)}</p></div><div class="actions">${m.queueId ? `<button type="button" class="btn" data-copy="${e(m.queueId)}">Copy queue ID</button>` : ''}${m.capture ? `<button type="button" class="btn primary" data-download="${m.id}">Download .eml</button>` : ''}</div></header><section class="panel"><dl class="facts">${facts.map(([title, value]) => `<div><dt>${title}</dt><dd>${value}</dd></div>`).join('')}</dl></section>`;
	const recipient = m.to ? `<section class="cards" aria-label="Recipients"><div class="panel rcpt"><div class="rcpt-head"><strong>${e(m.to)}</strong>${badge(m.status)}</div><span class="sub">${e(providerName(m.provider))} · ${attempts(m)} delivery attempts${!pending(m) && accepted(m) ? ` · took ${seconds(m.delay)}` : ''}</span><code class="raw">${e(attempt?.response ?? 'Waiting for the first delivery attempt')}</code></div></section>` : '';
	let content: string;
	if (c.route.tab === 'timeline') content = `<ol class="timeline">${m.events.map((ev) => `<li class="s-${e(ev.kind)}"><span class="when mono">${e(when(ev.at))}</span><div><strong>${e(ev.kind)}</strong> ${ev.dsn ? `<code>${e(ev.dsn)}</code>` : ''}<code class="line">${e(ev.program)}: ${e(m.queueId)}: ${e(ev.response)}</code></div></li>`).join('')}</ol>`;
	else if (c.route.tab === 'log') content = `<pre class="log">${e(m.events.map((ev: Event) => `${new Date(ev.at).toISOString()} ${m.server} ${ev.program}: ${m.queueId || 'NOQUEUE'}: ${ev.response}`).join('\n'))}</pre>`;
	else if (!m.capture) content = empty('Not captured. In the real app, captures are optional and have their own retention.');
	else if (c.route.tab === 'headers') content = `<dl class="headers">${messageHeaders(m).map(([key, value]) => `<dt class="mono">${key}</dt><dd class="mono">${e(value)}</dd>`).join('')}</dl>`;
	else content = `<div class="body-split"><div class="bodytext"><pre>${e(messageBody(m))}</pre></div><aside class="parts"><h3>Attachments</h3><p>None in this example.</p><p>All addresses, headers and content are made up.</p></aside></div>`;
	const tabs = `<div class="tabs-inline" aria-label="Message sections">${['timeline', 'headers', 'body', 'log'].map((tab) => `<a href="${e(link(c.route, `messages/${m.id}`, { tab }))}"${c.route.tab === tab ? ' aria-selected="true" aria-current="page"' : ''}>${({ timeline: 'Timeline', headers: 'Headers', body: 'Body', log: 'Log lines' }[tab])}</a>`).join('')}</div>`;
	return header + recipient + `<section class="panel" aria-label="Message content">${tabs}${content}</section>`;
}

export function render(c: Context) {
	if (c.route.view === 'messages' && c.route.id) return messageView(c);
	const views: Record<string, (c: Context) => string> = { overview, messages, queue, providers: providerView, domains: domainView, servers: serverView, alerts: alertView };
	return Object.hasOwn(views, c.route.view) ? views[c.route.view](c) : panel('Page not found', anchor(c, 'overview', 'Back to the overview'), '', false);
}
