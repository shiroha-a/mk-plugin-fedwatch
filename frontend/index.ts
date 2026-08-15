/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { definePlugin } from '@/plugin-api.js';
import { initApi } from './api.js';
import FederationPanel from './FederationPanel.vue';
import InstancePanel from './InstancePanel.vue';

export default definePlugin({
	name: 'fedwatch',

	setup(host) {
		initApi(host.api);

		// **独立したページは作らない。** 連合の状態を見に来るのは
		// コントロールパネルの「連合」なので、そこに出す方が導線が分かれない
		// (mk-go #2543)。
		//
		// このスロットはモデレーター向けのページにしか無いが、**描画される
		// ことは権限の保証ではない**。API 側は必ず IsModerator で弾いている。
		host.slot('admin:federation', { component: FederationPanel });

		// 個別のインスタンス情報には、その相手の応答率を 1 行で出す
		// (mk-go #2545)。ページが元から縦に長いので場所を取らない。
		host.slot('admin:instance-info', { component: InstancePanel });
	},
});
