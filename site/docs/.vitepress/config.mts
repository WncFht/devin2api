import { defineConfig } from 'vitepress'
import cnConfig from '../cn/config'

// https://vitepress.dev/reference/site-config
export default defineConfig({
	title: 'devin-2api',
	description:
		'An unofficial protocol adapter that exposes the models available to your Devin account behind OpenAI- and Anthropic-compatible endpoints.',
	base: '/devin2api/',
	rewrites: {
		'en/:rest*': ':rest*',
	},

	head: [
		['link', { rel: 'icon', type: 'image/svg+xml', href: '/devin2api/favicon.svg' }],
		['link', { rel: 'icon', type: 'image/x-icon', href: '/devin2api/favicon.ico' }],
		['link', { rel: 'apple-touch-icon', href: '/devin2api/apple-touch-icon.png' }],
	],

	themeConfig: {
		// https://vitepress.dev/reference/default-theme-config
		logo: '/logo.png',
		nav: [
			{ text: 'Home', link: '/' },
			{ text: 'Quick Start', link: '/introduction/quick-start' },
		],

		sidebar: [
			{
				text: 'Introduction',
				items: [
					{
						text: 'What is devin-2api?',
						link: '/introduction/what-is-devin-2api',
					},
					{ text: 'Quick Start', link: '/introduction/quick-start' },
					{
						text: 'GitHub',
						link: 'https://github.com/WncFht/devin2api',
					},
				],
			},
			{
				text: 'Installation',
				items: [
					{
						text: 'Install Script',
						link: '/installation/install-script',
					},
					{
						text: 'Deploy Scripts',
						link: '/installation/deploy-scripts',
					},
					{
						text: 'Prebuilt Binary',
						link: '/installation/prebuilt-binary',
					},
					{ text: 'Docker', link: '/installation/docker' },
					{ text: 'From Source', link: '/installation/from-source' },
					{ text: 'Upgrading', link: '/installation/upgrading' },
				],
			},
			{
				text: 'Client Setup',
				items: [
					{ text: 'Claude Code', link: '/clients/claude-code' },
					{ text: 'Codex', link: '/clients/codex' },
					{ text: 'Other Clients', link: '/clients/other-clients' },
				],
			},
			{
				text: 'Configuration',
				items: [
					{ text: 'Basic Configuration', link: '/configuration/basic' },
					{ text: 'Configuration Options', link: '/configuration/options' },
					{ text: 'Account Pool', link: '/configuration/account-pool' },
				],
			},
			{
				text: 'Management',
				items: [{ text: 'Admin Panel', link: '/management/panel' }],
			},
			{
				text: 'Troubleshooting',
				items: [{ text: 'Common Issues', link: '/troubleshooting' }],
			},
		],

		socialLinks: [
			{ icon: 'github', link: 'https://github.com/WncFht/devin2api' },
		],

		footer: {
			message: 'Released under the MIT License.',
			copyright: 'Copyright © 2026-present devin-2api contributors',
		},
	},

	locales: {
		root: {
			label: 'English',
			lang: 'en-US',
			link: '/',
		},
		cn: {
			label: '简体中文',
			lang: 'zh-Hans',
			link: '/cn',
			themeConfig: cnConfig.themeConfig,
		},
	},
})
