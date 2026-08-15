<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<!--
	1 行で済ませる。インスタンス情報のページは元から縦に長いので、
	ここで場所を取らない。**知りたいのは「どれだけ応答していたか」だけ。**
-->
<div v-if="data?.known" :class="$style.root">
	<span :class="$style.label">応答率</span>
	<span :class="[$style.value, $style[tone]]">{{ data.uptime }}%</span>
	<span :class="$style.meta">過去{{ windowLabel }}</span>
	<span v-if="data.outages > 0" :class="$style.meta">障害 {{ data.outages }} 回</span>
	<span v-if="data.notResponding" :class="$style.down">応答なし（{{ since(data.since) }}）</span>
</div>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { type SlotContext } from '@/plugin-api.js';
import { api, since, uptimeTone } from './api.js';
import type { HostStatus } from './api.js';

const props = defineProps<{ ctx: SlotContext }>();

const data = ref<HostStatus | null>(null);

const tone = computed(() => uptimeTone(data.value?.uptime ?? 100));

/*
 * 実際に測れた期間を出す。
 *
 * **「30 日で 100%」と「2 時間で 100%」は意味が違う。** 観測を始めたばかりの
 * 相手で「過去30日」と書くと嘘になるので、測れた分だけを言う。
 */
const windowLabel = computed(() => {
	const days = data.value?.days ?? 0;
	if (days < 1) return `${Math.max(1, Math.round(days * 24))}時間`;
	return `${Math.round(days)}日`;
});

onMounted(async () => {
	const host = props.ctx.host;
	if (host == null) return;

	try {
		data.value = await api<HostStatus>('host', { host });
	} catch {
		// 権限が無い / まだ観測していない場合は**何も出さない**。
		// このページには本体の情報があるので、空の行が挟まる方が邪魔になる。
	}
});
</script>

<style lang="scss" module>
.root {
	display: flex;
	align-items: baseline;
	flex-wrap: wrap;
	gap: 4px 10px;
	font-size: 0.9em;
}

.label {
	opacity: 0.7;
}

.value {
	font-weight: 700;
	font-variant-numeric: tabular-nums;
}

.good {
	color: var(--MI_THEME-success);
}

.warn {
	color: var(--MI_THEME-warn);
}

.bad {
	color: var(--MI_THEME-error);
}

.meta {
	opacity: 0.6;
	font-size: 0.9em;
}

.down {
	color: var(--MI_THEME-error);
	font-size: 0.9em;
}
</style>
