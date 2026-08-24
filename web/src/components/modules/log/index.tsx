import { useCallback, useEffect, useMemo, useState } from 'react';
import { Loader2, Logs } from 'lucide-react';
import { useTranslations } from 'use-intl';
import { useLogs, type RelayLogOverview } from '@/api/log';
import { useGroupList } from '@/api/group';
import { useModelChannelList } from '@/api/model';
import { useSessions } from '@/api/session';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';
import { modelChannelKey } from '@/components/modules/group/utils';
import { useSessionViewStore } from '@/stores/session';
import { SessionCard, type SessionBucket } from './Session';

// UNGROUPED_KEY 标识没有会话归属的请求所在的末尾分组。
const UNGROUPED_KEY = '__ungrouped__';

const EMPTY_LOGS: RelayLogOverview[] = [];

// Log 按会话分组展示进程内日志概览，并实时跟随会话与请求状态。
export function Log() {
    const t = useTranslations('log');
    const { logs, isLoading, error } = useLogs();
    const { sessions, error: sessionError } = useSessions();
    const { data: modelChannels = [] } = useModelChannelList();
    const { data: groups = [] } = useGroupList();
    const focusSessionId = useSessionViewStore((state) => state.focusSessionId);
    const clearFocusSession = useSessionViewStore((state) => state.clearFocusSession);
    const [collapsedOverrides, setCollapsedOverrides] = useState<Record<string, boolean>>({}); // 用户手动折叠或展开的会话，覆盖默认展开策略。

    // groupMemberKeys 提供分组成员集合，用于在切换器里把常用组合置顶。
    const groupMemberKeys = useMemo(() => {
        const map = new Map<string, Set<string>>();
        groups.forEach((group) => {
            map.set(group.name, new Set((group.items ?? []).map((item) => modelChannelKey(item.channel_id, item.model_name))));
        });
        return map;
    }, [groups]);

    // buckets 把请求按 session_id 归到会话下: 会话顺序跟随会话流的最近活跃倒序,
    // 会话内请求沿用日志流已有的 ID 倒序, 未归属的请求统一放在末尾。
    const buckets = useMemo(() => {
        const logsBySession = new Map<string, RelayLogOverview[]>();
        const ungrouped: RelayLogOverview[] = [];
        logs.forEach((log) => {
            if (!log.session_id) {
                ungrouped.push(log);
                return;
            }
            const existing = logsBySession.get(log.session_id);
            if (existing) existing.push(log);
            else logsBySession.set(log.session_id, [log]);
        });

        const result: SessionBucket[] = sessions.map((session) => {
            const sessionLogs = logsBySession.get(session.id);
            logsBySession.delete(session.id);
            return { key: session.id, session, logs: sessionLogs ?? EMPTY_LOGS };
        });
        // 会话流已清理但日志仍在的会话按日志自带的标签兜底展示。
        logsBySession.forEach((sessionLogs, id) => {
            result.push({ key: id, label: sessionLogs[0]?.session_label, logs: sessionLogs });
        });
        if (ungrouped.length > 0) result.push({ key: UNGROUPED_KEY, label: t('session.ungrouped'), logs: ungrouped });
        return result;
    }, [logs, sessions, t]);

    // 通知定位过来的高亮在几秒后自动解除，展开状态由渲染时直接推导。
    useEffect(() => {
        if (!focusSessionId) return;
        const timer = window.setTimeout(() => clearFocusSession(), 5000);
        return () => window.clearTimeout(timer);
    }, [clearFocusSession, focusSessionId]);

    const handleToggle = useCallback((key: string, next: boolean) => {
        setCollapsedOverrides((current) => ({ ...current, [key]: !next }));
    }, []);

    const streamError = error ?? sessionError;

    if (isLoading) {
        return (
            <div className="flex h-full items-center justify-center">
                <Loader2 className="size-6 animate-spin text-muted-foreground" />
            </div>
        );
    }

    if (buckets.length === 0) {
        return (
            <div className="flex h-full flex-col items-center justify-center gap-3 text-muted-foreground">
                {!streamError && <Logs className="size-8" />}
                <span className="text-sm">{streamError ? t('list.disconnected') : t('list.empty')}</span>
            </div>
        );
    }

    return (
        <div className="flex h-full min-h-0 flex-col gap-3">
            {streamError && (
                <div className="flex shrink-0 items-center justify-center px-1 pb-3 text-xs text-destructive">
                    <span>{t('list.disconnected')}</span>
                </div>
            )}
            <div className="min-h-0 flex-1">
                <VirtualizedGrid
                    items={buckets}
                    layout="list"
                    columns={{ default: 1 }}
                    estimateItemHeight={220}
                    overscan={4}
                    getItemKey={(bucket) => `session-${bucket.key}`}
                    scrollToKey={focusSessionId ? `session-${focusSessionId}` : null}
                    renderItem={(bucket, index) => {
                        // 等待选择渠道、通知定位过来和最近活跃的会话默认展开，其余保持折叠。
                        const focused = focusSessionId === bucket.key;
                        const defaultExpanded = focused || bucket.session?.status === 'pending' || index === 0;
                        const collapsed = collapsedOverrides[bucket.key];
                        return (
                            <SessionCard
                                bucket={bucket}
                                modelChannels={modelChannels}
                                groupMemberKeys={groupMemberKeys}
                                expanded={collapsed === undefined || focused ? defaultExpanded : !collapsed}
                                highlighted={focused}
                                onToggle={handleToggle}
                            />
                        );
                    }}
                />
            </div>
        </div>
    );
}
