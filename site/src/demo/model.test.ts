import assert from 'node:assert/strict';
import test from 'node:test';
import { accepted, advance, createDemo, filterMessages, pending, periods, totals, type Filters } from './model.ts';
import { link, readRoute, render } from './views.ts';

const now = Date.UTC(2026, 9, 19, 12);
const filters: Filters = { site: '', server: '', period: '24h', q: '' };

test('traffic, queue and totals agree, including rejected and cancelled SMTP transactions', () => {
	const messages = createDemo(now);
	assert.equal(messages.length, 1260);
	assert.equal(new Set(messages.map((m) => m.id)).size, messages.length);
	const rows = filterMessages(messages, filters, now), counts = totals(rows);
	assert.ok(counts.sent > 0 && counts.deferrals > 0 && counts.bounced > 0 && counts.rejected > 0);
	assert.equal(counts.received, rows.filter(accepted).length);
	assert.equal(counts.received + counts.rejected + rows.filter((m) => m.status === 'cancelled').length, rows.length);
	assert.equal(counts.queue, rows.filter(pending).length);
	assert.ok(messages.every((m) => m.events.every((ev) => ev.at <= now)));
	const cancelled = messages.find((m) => m.id === 6)!;
	assert.equal(cancelled.status, 'cancelled');
	assert.equal(cancelled.sender, null);
	assert.ok(!pending(cancelled) && !accepted(cancelled));
});

test('site and server filters keep the matching app and have a separate positive control', () => {
	const messages = createDemo(now);
	const storefront = filterMessages(messages, { ...filters, site: 'storefront', server: 'relay-2' }, now);
	assert.ok(storefront.some((m) => m.id === 1));
	assert.ok(storefront.every((m) => m.site === 'storefront' && m.server === 'relay-2'));
	assert.ok(!storefront.some((m) => m.id === 2));
	assert.ok(filterMessages(messages, { ...filters, site: 'accounts', server: 'relay-1' }, now).some((m) => m.id === 2));
});

test('search combines fields, quoted subjects, negation, DSNs and time ranges', () => {
	const messages = createDemo(now);
	const find = (q: string) => filterMessages(messages, { ...filters, q }, now);
	assert.ok(find('provider:microsoft status:deferred subject:"Your order"').some((m) => m.id === 1));
	assert.ok(!find('provider:microsoft status:deferred').some((m) => m.id === 2));
	assert.ok(find('provider:google status:sent').some((m) => m.id === 2));
	assert.ok(find('dsn:4.7 -status:sent').every((m) => m.status === 'deferred'));
	assert.ok(find('has:capture from:accounts.example').some((m) => m.id === 2));
	assert.equal(find('a-word-that-is-not-in-the-data').length, 0);
	assert.ok(find('after:7d').length > find('').length);
	const old = find('after:7d before:1d');
	assert.ok(old.length > 0 && old.every((m) => m.at >= now - periods['7d'] && m.at < now - periods['24h']));
});

test('queue ignores the period picker and live arrivals resolve consistently', () => {
	const messages = createDemo(now);
	assert.deepEqual(filterMessages(messages, { ...filters, period: '1h' }, now, true).map((m) => m.id), filterMessages(messages, { ...filters, period: '30d' }, now, true).map((m) => m.id));
	const count = messages.length;
	advance(messages, now + 5000, 'throttling');
	assert.equal(messages.length, count + 1);
	assert.equal(messages[0].status, 'queued');
	assert.equal(messages.find((m) => m.id === 5)!.status === 'queued', false);
	advance(messages, now + 10000, 'healthy');
	const delivered = messages.find((m) => m.id === 1261)!;
	assert.equal(delivered.status, 'sent');
	assert.equal(delivered.events.at(-1)!.kind, 'removed');
});

test('healthy scenario clears the active delay; DNS failures agree across domains and alerts', () => {
	assert.ok(createDemo(now, 'throttling').some((m) => m.status === 'deferred'));
	assert.ok(!createDemo(now, 'healthy').some((m) => m.status === 'deferred'));
	const context = { messages: createDemo(now, 'dns'), now, scenario: 'dns' as const, route: readRoute('#/domains') };
	assert.match(render(context), /No DKIM record found/);
	assert.match(render({ ...context, route: readRoute('#/alerts') }), />firing<\/span>/);
	assert.doesNotMatch(render({ ...context, scenario: 'healthy', route: readRoute('#/domains') }), /No DKIM record found/);
});

test('routes preserve filters, survive unknown pages and treat search text as text', () => {
	const route = readRoute('#/messages?site=storefront&server=relay-2&period=7d&q=status%3Adeferred');
	const next = readRoute(link(route, 'messages/1', { tab: 'headers' }));
	assert.equal(next.site, route.site); assert.equal(next.server, route.server); assert.equal(next.period, '7d'); assert.equal(next.tab, 'headers'); assert.equal(next.id, 1);
	const context = { messages: createDemo(now), now, scenario: 'throttling' as const, route: readRoute('#/__proto__') };
	assert.match(render(context), /Page not found/);
	const malicious = '<img src=x onerror=alert(1)>';
	const html = render({ ...context, route: readRoute(link(route, 'messages', { q: malicious })) });
	assert.ok(html.includes('&lt;img'));
	assert.ok(!html.includes(malicious));
	const control = render({ ...context, route: readRoute('#/messages/2?tab=body') });
	assert.match(control, /This is a made-up message/);
	assert.ok(!control.includes('Not captured.'));
});
