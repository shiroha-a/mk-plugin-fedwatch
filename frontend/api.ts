/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

let call: <T>(endpoint: string, params?: Record<string, unknown>) => Promise<T>;

/** Called once from the plugin's setup. */
export function initApi(fn: typeof call): void {
	call = fn;
}

export function api<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
	return call<T>(`plugin/fedwatch/${path}`, params);
}

export type DownInstance = {
	host: string;
	softwareName: string;
	softwareVersion: string;
	/** 応答しなくなった時刻。 */
	since: string;
	/** こちらが相手の利用者をフォローしている数。実害の大きさの目安。 */
	followingCount: number;
	followersCount: number;
	suspended: boolean;
	blocked: boolean;
};

export type SoftwareCount = {
	name: string;
	count: number;
	/** そのうち応答していない数。 */
	down: number;
};

export type EventRow = {
	host: string;
	/** down / up / suspended / unsuspended / blocked / unblocked / version / appeared */
	kind: string;
	detail: string;
	at: string;
};

export type Overview = {
	total: number;
	down: number;
	suspended: number;
	blocked: number;
	downList: DownInstance[];
	software: SoftwareCount[];
	events: EventRow[];
	/** 最後に取り込んだ時刻。まだ 1 度も動いていなければ null。 */
	lastRun: string | null;
};

const EVENT_LABELS: Record<string, string> = {
	down: '応答しなくなった',
	up: '復旧した',
	suspended: '停止した',
	unsuspended: '停止を解除した',
	blocked: 'ブロックした',
	unblocked: 'ブロックを解除した',
	version: 'バージョンが変わった',
	appeared: '観測を始めた',
};

export function eventLabel(kind: string): string {
	return EVENT_LABELS[kind] ?? kind;
}

/** 出来事の種類を色で分ける。復旧は目立たなくてよい。 */
export function eventTone(kind: string): 'bad' | 'good' | 'plain' {
	switch (kind) {
		case 'down':
		case 'suspended':
		case 'blocked':
			return 'bad';
		case 'up':
		case 'unsuspended':
		case 'unblocked':
			return 'good';
		default:
			return 'plain';
	}
}

/*
 * 経過時間。プラグインからは MkTime を使えないので自前で組む。
 *
 * 「いつから落ちているか」が読めればよいので粒度は粗くてよい。
 *
 * **出来上がった文字列を後から削って別の表記を作らない。** 以前は
 * `since()` の「〜から」を `slice` で落として `ago()` を作っていたが、
 * 落とす文字数を間違えたうえに呼び出し側でも「前」を足していて
 * 「5分前か前」と出ていた。単位までを 1 か所で組み、語尾は各関数が付ける。
 */

/** 「5分」「3時間」「2日」。語尾は付けない。 */
function elapsed(iso: string): string {
	const diff = (Date.now() - new Date(iso).getTime()) / 1000;
	if (!Number.isFinite(diff) || diff < 0) return '';
	if (diff < 60) return 'たった今';
	if (diff < 3600) return `${Math.floor(diff / 60)}分`;
	if (diff < 86400) return `${Math.floor(diff / 3600)}時間`;
	if (diff < 86400 * 30) return `${Math.floor(diff / 86400)}日`;
	return `${Math.floor(diff / (86400 * 30))}か月`;
}

/** 「5分前から」。いつからその状態が続いているかを言う。 */
export function since(iso: string): string {
	const s = elapsed(iso);
	if (s === '') return '';
	if (s === 'たった今') return 'たった今から';
	return `${s}前から`;
}

/** 「5分前」。いつのことかを言う。**呼び出し側で「前」を足さないこと。** */
export function ago(iso: string): string {
	const s = elapsed(iso);
	if (s === '' || s === 'たった今') return s;
	return `${s}前`;
}

export type HostStatus = {
	host: string;
	/** 観測しているか。連合し始めたばかりなら false。 */
	known: boolean;
	notResponding: boolean;
	softwareName: string;
	softwareVersion: string;
	/** 今の状態になった時刻。 */
	since: string;
	/** 測定期間のうち応答していた割合 (0-100)。 */
	uptime: number;
	/** 実際に測れた日数。観測開始が浅ければ短くなる。 */
	days: number;
	/** 期間中に応答しなくなった回数。 */
	outages: number;
};

/** 応答率の色分け。落ちている相手を見落とさない程度に。 */
export function uptimeTone(uptime: number): 'bad' | 'warn' | 'good' {
	if (uptime < 90) return 'bad';
	if (uptime < 99) return 'warn';
	return 'good';
}
