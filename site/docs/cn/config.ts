import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
	title: 'devin-2api',
	description:
		'把 Devin 账号可用的模型挂到 OpenAI 与 Anthropic 兼容端点之后的非官方协议适配器。',
	themeConfig: {
		// https://vitepress.dev/reference/default-theme-config
		nav: [
			{ text: '首页', link: '/cn/' },
			{ text: '快速开始', link: '/cn/introduction/quick-start' },
		],

		sidebar: [
			{
				text: '介绍',
				items: [
					{
						text: 'devin-2api 是什么？',
						link: '/cn/introduction/what-is-devin-2api',
					},
					{ text: '快速开始', link: '/cn/introduction/quick-start' },
					{
						text: 'GitHub',
						link: 'https://github.com/WncFht/devin2api',
					},
				],
			},
			{
				text: '安装',
				items: [
					{ text: '一键脚本', link: '/cn/installation/install-script' },
					{ text: '部署脚本', link: '/cn/installation/deploy-scripts' },
					{
						text: '预编译二进制',
						link: '/cn/installation/prebuilt-binary',
					},
					{ text: 'Docker', link: '/cn/installation/docker' },
					{ text: '源码构建', link: '/cn/installation/from-source' },
					{ text: '升级', link: '/cn/installation/upgrading' },
				],
			},
			{
				text: '客户端接入',
				items: [
					{ text: 'Claude Code', link: '/cn/clients/claude-code' },
					{ text: 'Codex', link: '/cn/clients/codex' },
					{ text: '其他客户端', link: '/cn/clients/other-clients' },
				],
			},
			{
				text: '配置',
				items: [
					{ text: '基础配置', link: '/cn/configuration/basic' },
					{ text: '配置项', link: '/cn/configuration/options' },
					{ text: '账号池', link: '/cn/configuration/account-pool' },
				],
			},
			{
				text: '管理',
				items: [{ text: '管理面板', link: '/cn/management/panel' }],
			},
			{
				text: '排障',
				items: [{ text: '常见问题', link: '/cn/troubleshooting' }],
			},
		],

		docFooter: {
			prev: '上一页',
			next: '下一页',
		},
		outline: {
			label: '本页内容',
		},
		langMenuLabel: '语言',
		returnToTopLabel: '回到顶部',
		sidebarMenuLabel: '菜单',
		darkModeSwitchLabel: '主题',
		lightModeSwitchTitle: '浅色模式',
		darkModeSwitchTitle: '深色模式',
		footer: {
			message: '基于 MIT 协议发布。',
			copyright: 'Copyright © 2026-present devin-2api contributors',
		},
	},
})
