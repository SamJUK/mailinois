// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// GitHub Pages: site is https://<user>.github.io and base is '/<repo>' ('/mailinois').
// With a custom domain, set site to that domain and base back to '/'.
const site = 'https://samjuk.github.io';
const base = '/mailinois';

export default defineConfig({
	site,
	base,
	integrations: [
		starlight({
			title: 'Mailinois',
			description:
				'Self-hosted Postfix observability: follow every message by queue ID, from the app that handed it over to each recipient’s final answer.',
			logo: { src: './src/assets/logo.svg' },
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/SamJUK/mailinois' }],
			components: { Hero: './src/components/Hero.astro' },
			customCss: [
				'@fontsource-variable/geist',
				'@fontsource-variable/geist-mono',
				'./src/styles/theme.css',
			],
			sidebar: [
				{ label: 'Start here', items: ['docs/introduction', 'docs/browser-demo', 'docs/quick-start', 'docs/report', 'docs/install'] },
				{
					label: 'Set up',
					items: [
						'docs/configuration',
						'docs/databases',
						'docs/sending-logs',
						'docs/kubernetes',
						'docs/works-with',
						'docs/capture',
						'docs/authentication',
					],
				},
				{
					label: 'Use',
					items: [
						'docs/sites-and-search',
						'docs/alerts',
						'docs/api',
						'docs/dns-checks',
						'docs/metrics',
					],
				},
				{ label: 'Background', items: ['docs/privacy-and-security', 'docs/how-it-works', 'docs/sizing'] },
			],
		}),
	],
});
