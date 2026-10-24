import { advance, createDemo, filterMessages, type Scenario } from './model.ts';
import { escape, labels, link, messageBody, messageHeaders, readRoute, render, tail, type Context } from './views.ts';

const main = document.querySelector<HTMLElement>('#demo-main')!;
const search = document.querySelector<HTMLInputElement>('#search')!;
const site = document.querySelector<HTMLSelectElement>('#site-filter')!;
const server = document.querySelector<HTMLSelectElement>('#server-filter')!;
const scenarioPicker = document.querySelector<HTMLSelectElement>('#scenario')!;
const pauseButton = document.querySelector<HTMLButtonElement>('#simulation-toggle')!;
const themeButton = document.querySelector<HTMLButtonElement>('#theme-toggle')!;
const announcement = document.querySelector<HTMLElement>('#demo-announcement')!;
let scenario: Scenario = 'throttling';
let messages = createDemo();
let paused = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
let route = readRoute(location.hash);

const context = (): Context => ({ route, messages, now: Date.now(), scenario });
const announce = (text: string) => { announcement.textContent = text; };
function setSimulationState() {
	pauseButton.textContent = paused ? 'Resume simulation' : 'Pause simulation';
	pauseButton.setAttribute('aria-pressed', String(paused));
	document.querySelector('#simulation-status')!.textContent = paused ? 'Simulation paused' : 'Simulation running';
	document.querySelector('#simulation-light')!.classList.toggle('on', !paused);
}
function setTheme(theme: string) {
	document.documentElement.dataset.theme = theme === 'light' ? 'light' : 'dark';
	themeButton.setAttribute('aria-label', `Switch to ${theme === 'light' ? 'dark' : 'light'} theme`);
	try { localStorage.setItem('mailinois-demo-theme', theme); } catch { /* The demo also works with storage disabled. */ }
}
function updateCounts() {
	document.querySelector('#queue-count')!.textContent = String(filterMessages(messages, { ...route, q: '' }, Date.now(), true).length);
}
function draw() {
	const c = context();
	const title = route.id ? 'Message details' : Object.hasOwn(labels, route.view) ? labels[route.view] : 'Page not found';
	main.innerHTML = `<h1 class="sr-only">${escape(title)}</h1>${render(c)}`;
	search.value = route.q;
	site.value = route.site;
	server.value = route.server;
	for (const item of document.querySelectorAll<HTMLAnchorElement>('[data-view]')) {
		item.href = link(route, item.dataset.view);
		if (item.dataset.view === route.view) item.setAttribute('aria-current', 'page');
		else item.removeAttribute('aria-current');
	}
	for (const item of document.querySelectorAll<HTMLAnchorElement>('[data-period]')) {
		item.href = link(route, route.id ? `messages/${route.id}` : route.view, { period: item.dataset.period!, q: route.q, tab: route.tab });
		if (item.dataset.period === route.period) item.setAttribute('aria-current', 'true');
		else item.removeAttribute('aria-current');
	}
	updateCounts();
	document.title = `${title} · Demo · Mailinois`;
}
function navigate(hash: string) {
	if (location.hash === hash) { route = readRoute(hash); draw(); }
	else location.hash = hash;
}
window.addEventListener('hashchange', () => {
	if (location.hash === '#demo-main') { main.focus(); return; }
	const previous = route;
	route = readRoute(location.hash);
	draw();
	if (previous.view !== route.view || previous.id !== route.id) {
		main.focus({ preventScroll: true });
		window.scrollTo({ top: 0, behavior: 'instant' });
	}
});
document.querySelector('#demo-search')!.addEventListener('submit', (event) => {
	event.preventDefault(); navigate(link(route, 'messages', { q: search.value.trim() }));
});
for (const [control, field] of [[site, 'site'], [server, 'server']] as const) control.addEventListener('change', () => navigate(link(route, route.id ? 'messages' : route.view, { [field]: control.value, q: route.q })));
pauseButton.addEventListener('click', () => { paused = !paused; setSimulationState(); announce(paused ? 'Simulation paused.' : 'Simulation resumed.'); });
scenarioPicker.addEventListener('change', () => {
	scenario = scenarioPicker.value as Scenario;
	messages = createDemo(Date.now(), scenario);
	draw(); announce(`Scenario changed to ${scenarioPicker.selectedOptions[0].textContent}.`);
});
document.querySelector('#simulation-reset')!.addEventListener('click', () => {
	scenario = 'throttling'; scenarioPicker.value = scenario;
	messages = createDemo(); paused = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
	setSimulationState(); navigate('#/'); announce('Demo reset to the original simulated traffic.');
});
themeButton.addEventListener('click', () => setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'));
document.addEventListener('keydown', (event) => {
	if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); search.focus(); search.select(); }
});
main.addEventListener('click', async (event) => {
	const target = (event.target as HTMLElement).closest<HTMLButtonElement>('button');
	if (target?.dataset.copy) {
		try { await navigator.clipboard.writeText(target.dataset.copy); target.textContent = 'Copied'; announce('Queue ID copied.'); }
		catch { announce('Clipboard unavailable. The queue ID is shown in the message details.'); }
	}
	if (target?.dataset.download) {
		const message = messages.find((m) => m.id === Number(target.dataset.download));
		if (!message?.capture) return;
		const body = messageHeaders(message).map(([name, value]) => `${name}: ${value}`).join('\r\n') + '\r\n\r\n' + messageBody(message).replace(/\n/g, '\r\n');
		const url = URL.createObjectURL(new Blob([body], { type: 'message/rfc822' }));
		const anchor = document.createElement('a'); anchor.href = url; anchor.download = `demo-${message.queueId}.eml`; anchor.click();
		setTimeout(() => URL.revokeObjectURL(url), 1000); announce('Simulated message downloaded.');
	}
});
setInterval(() => {
	if (paused || document.hidden) return;
	advance(messages, Date.now(), scenario); updateCounts();
	if (route.view === 'overview') {
		if (!main.contains(document.activeElement)) draw();
		else { const list = document.querySelector('#demo-tail'); if (list) list.innerHTML = tail(context()); }
	}
}, 4000);
try { setTheme(localStorage.getItem('mailinois-demo-theme') ?? 'dark'); } catch { setTheme('dark'); }
setSimulationState(); draw();
