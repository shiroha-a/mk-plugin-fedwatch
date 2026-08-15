<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<!--
	**全体を畳んでおく。** このページの主役は本体のインスタンス検索なので、
	プラグインが足した部分で画面を占有しない。畳んだままでも状態が分かるよう、
	要約を suffix に出す。
-->
<MkFolder v-if="!hidden" :class="$style.root">
	<template #icon><i class="ti ti-heartbeat"></i></template>
	<template #label>連合の健康状態</template>
	<template #suffix>{{ summary }}</template>

	<div class="_gaps_s">
	<div v-if="error" :class="$style.error">{{ error }}</div>

	<template v-else-if="data">
		<div :class="$style.cards">
			<div :class="$style.card">
				<div :class="$style.cardValue">{{ data.total }}</div>
				<!--
					観測した相手をすべて数えても運用の役に立たない。数えるのは
					やり取りが続いている相手だけ (本体の federating と同じ定義)。
				-->
				<div :class="$style.cardLabel">連合中</div>
			</div>
			<div :class="[$style.card, { [$style.cardBad]: data.down > 0 }]">
				<div :class="$style.cardValue">{{ data.down }}</div>
				<div :class="$style.cardLabel">応答なし</div>
			</div>
			<div :class="$style.card">
				<div :class="$style.cardValue">{{ data.suspended }}</div>
				<div :class="$style.cardLabel">停止中</div>
			</div>
			<div :class="$style.card">
				<div :class="$style.cardValue">{{ data.blocked }}</div>
				<div :class="$style.cardLabel">ブロック</div>
			</div>
		</div>

		<!--
			一覧は畳んでおく。このページの主役はインスタンスの検索なので、
			状態の詳細で画面を埋めない。
		-->
		<MkFolder v-if="data.downList.length > 0">
			<template #icon><i class="ti ti-plug-connected-x"></i></template>
			<template #label>応答していない相手</template>
			<template #suffix>{{ data.downList.length }}</template>

			<div :class="$style.list">
				<div v-for="d in data.downList" :key="d.host" :class="$style.row">
					<div :class="$style.rowMain">
						<span :class="$style.host">{{ d.host }}</span>
						<span v-if="d.softwareName" :class="$style.meta">{{ d.softwareName }} {{ d.softwareVersion }}</span>
						<span v-if="d.suspended" :class="$style.tag">停止中</span>
						<span v-if="d.blocked" :class="$style.tag">ブロック</span>
					</div>
					<div :class="$style.rowSide">
						<!-- フォローしている人がいる相手は実害が出ているので目立たせる。 -->
						<span v-if="d.followingCount > 0" :class="$style.impact">フォロー {{ d.followingCount }}</span>
						<span :class="$style.when">{{ since(d.since) }}</span>
					</div>
				</div>
			</div>
		</MkFolder>

		<MkFolder v-if="data.events.length > 0">
			<template #icon><i class="ti ti-history"></i></template>
			<template #label>最近の変化</template>

			<div :class="$style.list">
				<div v-for="(e, i) in data.events" :key="i" :class="$style.row">
					<div :class="$style.rowMain">
						<span :class="[$style.dot, $style[eventTone(e.kind)]]"></span>
						<span :class="$style.host">{{ e.host }}</span>
						<span :class="$style.meta">{{ eventLabel(e.kind) }}</span>
						<span v-if="e.detail" :class="$style.meta">{{ e.detail }}</span>
					</div>
					<div :class="$style.rowSide">
						<span :class="$style.when">{{ ago(e.at) }}</span>
					</div>
				</div>
			</div>
		</MkFolder>

		<div :class="$style.foot">
			<span v-if="data.lastRun == null" :class="$style.note">
				まだ取り込んでいません。毎時 7 分に自動で走ります。
			</span>
			<span v-else :class="$style.note">最終取り込み: {{ ago(data.lastRun) }}</span>
			<MkButton :disabled="collecting" @click="collectNow">
				<template v-if="collecting"><MkLoading :em="true"/></template>
				<template v-else>今すぐ取り込む</template>
			</MkButton>
		</div>
	</template>
	</div>
</MkFolder>
</template>

<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import { MkButton, MkFolder, MkLoading } from '@/plugin-api.js';
import { api, eventLabel, eventTone, since, ago } from './api.js';
import type { Overview } from './api.js';

const data = ref<Overview | null>(null);
const collecting = ref(false);
const error = ref('');
// 権限が無い / プラグインが止まっている場合は**何も出さない**。
// このページには本体の一覧があるので、空の枠が挟まる方が邪魔になる。
const hidden = ref(true);

// 畳んだままでも状態が分かるようにする。**開かないと分からない**なら、
// 畳む意味がない。
const summary = computed(() => {
	if (error.value) return 'エラー';
	if (data.value == null) return '';
	if (data.value.down === 0) return `${data.value.total}`;
	return `${data.value.total} / 応答なし ${data.value.down}`;
});

async function load(): Promise<void> {
	try {
		data.value = await api<Overview>('overview', {});
		error.value = '';
		hidden.value = false;
	} catch (err) {
		const message = (err as { message?: string } | null)?.message ?? '';
		// 403 は「モデレーターでない」なので黙って消える。それ以外は理由を出す
		// (読み込み失敗と権限不足で対応が変わる)。
		if (message.includes('権限')) {
			hidden.value = true;
			return;
		}
		error.value = message || '連合の状態を読み込めませんでした';
		hidden.value = false;
	}
}

async function collectNow(): Promise<void> {
	collecting.value = true;
	try {
		await api('collect', {});
		await load();
	} catch (err) {
		error.value = (err as { message?: string } | null)?.message ?? '取り込みに失敗しました';
	} finally {
		collecting.value = false;
	}
}

onMounted(load);
</script>

<style lang="scss" module>
.root {
	margin-bottom: 8px;
}

.error {
	color: var(--MI_THEME-error);
	font-size: 0.9em;
}

.cards {
	display: grid;
	grid-template-columns: repeat(auto-fit, minmax(110px, 1fr));
	gap: 8px;
}

.card {
	padding: 10px;
	border-radius: var(--MI-radius, 12px);
	background: var(--MI_THEME-panel);
	border: 1px solid var(--MI_THEME-divider);
	text-align: center;
}

/* 応答なしが 1 件でもあれば目に入るようにする。 */
.cardBad {
	border-color: var(--MI_THEME-error);
}

.cardValue {
	font-size: 1.5em;
	font-weight: 700;
	font-variant-numeric: tabular-nums;
	line-height: 1.2;
}

.cardLabel {
	font-size: 0.8em;
	opacity: 0.7;
}

.list {
	display: flex;
	flex-direction: column;
}

.row {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: 12px;
	padding: 6px 2px;
	font-size: 0.9em;

	& + & {
		border-top: 1px solid var(--MI_THEME-divider);
	}
}

.rowMain {
	display: flex;
	align-items: center;
	gap: 8px;
	min-width: 0;
}

.rowSide {
	display: flex;
	align-items: center;
	gap: 10px;
	flex-shrink: 0;
}

.host {
	font-weight: 600;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.meta {
	opacity: 0.65;
	font-size: 0.9em;
	overflow: hidden;
	text-overflow: ellipsis;
	white-space: nowrap;
}

.tag {
	padding: 0 6px;
	border-radius: 999px;
	background: var(--MI_THEME-buttonBg);
	font-size: 0.8em;
	flex-shrink: 0;
}

.impact {
	color: var(--MI_THEME-warn);
	font-size: 0.85em;
}

.when {
	opacity: 0.55;
	font-size: 0.85em;
}

.dot {
	width: 8px;
	height: 8px;
	border-radius: 100%;
	flex-shrink: 0;
	background: var(--MI_THEME-fg);
}

.bad {
	background: var(--MI_THEME-error);
}

.good {
	background: var(--MI_THEME-success);
}

.plain {
	opacity: 0.4;
}

.foot {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: 12px;
}

.note {
	font-size: 0.85em;
	opacity: 0.7;
}
</style>
