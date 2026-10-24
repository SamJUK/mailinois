const base = import.meta.env.BASE_URL.replace(/\/$/, '');

export function withBase(path: string): string {
	if (/^([a-z]+:)?\/\//i.test(path) || path.startsWith('#')) return path;
	return `${base}/${path.replace(/^\//, '')}`;
}
