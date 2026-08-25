import { useMutation } from '@tanstack/react-query';
import { useEffect, useSyncExternalStore } from 'react';
import { apiRequest } from './client';
import type { RelayUsage } from './log';

// SessionStatus 表示会话当前的选路状态。
export type SessionStatus = 'pending' | 'bound' | 'error';

// SessionOverview 是会话状态流发送的完整进程内会话状态。
export interface SessionOverview {
    id: string;
    label: string;
    client: string;
    requested_model: string; // 客户端本次请求的模型名, 仅供展示。
    status: SessionStatus;
    channel_id: number;
    channel_name: string;
    model_name: string;
    started_at: string;
    last_active_at: string;
    request_count: number;
    pending_count: number;
    usage: RelayUsage;
    cost: number;
    error?: string;
}

// SessionStreamSnapshot 是共享订阅对外暴露的不可变快照。
interface SessionStreamSnapshot {
    sessions: SessionOverview[];
    isLoading: boolean;
    error: Error | null;
}

const emptySnapshot: SessionStreamSnapshot = { sessions: [], isLoading: true, error: null };

// 会话流在整个应用内只保留一条连接: 通知层和日志页共享同一份快照,
// 由引用计数决定何时建立和关闭 EventSource。
let snapshot: SessionStreamSnapshot = emptySnapshot;
let source: EventSource | null = null;
let refCount = 0;
const listeners = new Set<() => void>();

// activeAt 解析会话最后活跃时间, 无法解析时视为最早时间。
function activeAt(session: SessionOverview) {
    const time = Date.parse(session.last_active_at);
    return Number.isNaN(time) ? 0 : time;
}

// upsertSession 按 ID 合并会话并维持 last_active_at 倒序,
// 活跃时间未变化时原地替换, 由此避免每条更新重排整个列表。
function upsertSession(current: SessionOverview[], next: SessionOverview) {
    const nextActiveAt = activeAt(next);
    const index = current.findIndex((item) => item.id === next.id);
    if (index >= 0 && activeAt(current[index]) === nextActiveAt) {
        const updated = current.slice();
        updated[index] = next;
        return updated;
    }
    const rest = index >= 0 ? [...current.slice(0, index), ...current.slice(index + 1)] : current;
    const position = rest.findIndex((item) => activeAt(item) < nextActiveAt);
    if (position < 0) return [...rest, next];
    return [...rest.slice(0, position), next, ...rest.slice(position)];
}

// publish 提交新的共享快照并通知全部订阅者。
function publish(patch: Partial<SessionStreamSnapshot>) {
    snapshot = { ...snapshot, ...patch };
    listeners.forEach((listener) => listener());
}

// openSessionStream 建立唯一的会话状态流连接。
function openSessionStream() {
    const stream = new EventSource('/api/v1/session/stream', { withCredentials: true });
    source = stream;

    stream.onopen = () => publish({ isLoading: false, error: null });
    stream.addEventListener('session', (event) => {
        let next: SessionOverview;
        try {
            next = JSON.parse((event as MessageEvent<string>).data) as SessionOverview;
        } catch {
            publish({ error: new Error('Invalid session update') });
            return;
        }
        publish({ sessions: upsertSession(snapshot.sessions, next), isLoading: false, error: null });
    });
    stream.onerror = () => publish({ isLoading: false, error: new Error('Session stream disconnected') });
}

// retainSessionStream 在首个订阅者接入时建立共享连接。
function retainSessionStream() {
    refCount += 1;
    if (refCount > 1 || source) return;
    openSessionStream();
}

// releaseSessionStream 在最后一个订阅者退出时关闭共享连接并清空快照。
function releaseSessionStream() {
    refCount = Math.max(0, refCount - 1);
    if (refCount > 0) return;
    source?.close();
    source = null;
    publish(emptySnapshot);
}

function subscribeSessionStream(listener: () => void) {
    listeners.add(listener);
    return () => {
        listeners.delete(listener);
    };
}

function getSessionSnapshot() {
    return snapshot;
}

// useSessions 订阅共享会话流, 并按最近活跃倒序输出会话列表。
export function useSessions() {
    const state = useSyncExternalStore(subscribeSessionStream, getSessionSnapshot, getSessionSnapshot);

    useEffect(() => {
        retainSessionStream();
        return releaseSessionStream;
    }, []);

    return state;
}

// useBindSession 为会话绑定渠道和模型, 运行中的会话会立即改道。
export function useBindSession() {
    return useMutation({
        mutationFn: ({ sessionId, channelId, modelName }: { sessionId: string; channelId: number; modelName: string }) =>
            apiRequest<null>('/api/v1/session/bind', {
                method: 'POST',
                body: { session_id: sessionId, channel_id: channelId, model_name: modelName },
            }),
    });
}

// useUnbindSession 解除会话绑定, 该会话下一条请求会重新等待选择。
export function useUnbindSession() {
    return useMutation({
        mutationFn: ({ sessionId }: { sessionId: string }) =>
            apiRequest<null>('/api/v1/session/unbind', {
                method: 'POST',
                body: { session_id: sessionId },
            }),
    });
}
