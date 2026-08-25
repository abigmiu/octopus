import { memo, useCallback, useEffect, useMemo, useState } from 'react';
import {
    AlertCircle,
    ArrowDownToLine,
    ArrowUpFromLine,
    Check,
    ChevronDown,
    DollarSign,
    Hash,
    Loader2,
    Search,
    Timer,
    Unlink,
} from 'lucide-react';
import { useTranslations } from 'use-intl';
import { toast } from 'sonner';
import { type RelayLogOverview } from '@/api/log';
import { useBindSession, useUnbindSession, type SessionOverview } from '@/api/session';
import type { LLMChannel } from '@/api/model';
import { getModelIcon } from '@/lib/model-icons';
import { cn } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { Input } from '@/components/ui/input';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { memberKey } from '@/lib/model-channel';
import { LogCard } from './Item';

// PENDING_TIMEOUT_MS 是后端让等待中的请求失败前留给管理员的选择窗口。
const PENDING_TIMEOUT_MS = 60_000;

// CLIENT_LABEL_KEYS 将后端下发的客户端标识映射为文案键。
const CLIENT_LABEL_KEYS: Record<string, string> = {
    'claude-code': 'claudeCode',
    codex: 'codex',
    'openai-sdk': 'openaiSdk',
    unknown: 'unknown',
};

// SessionBucket 是一个会话及其归属请求，会话流已不再下发时退化为日志自带的标签。
export interface SessionBucket {
    key: string;
    session?: SessionOverview;
    label?: string;
    logs: RelayLogOverview[];
}

// SessionChannelPicker 渲染可搜索的渠道模型选择列表。
function SessionChannelPicker({
    options,
    currentKey,
    disabled,
    isSubmitting,
    triggerLabel,
    triggerHint,
    onSelect,
}: {
    options: LLMChannel[];
    currentKey: string;
    disabled: boolean;
    isSubmitting: boolean;
    triggerLabel: string;
    triggerHint: string;
    onSelect: (option: LLMChannel) => void;
}) {
    const t = useTranslations('log.session');
    const [open, setOpen] = useState(false);
    const [keyword, setKeyword] = useState('');
    const { Icon, className: iconClassName } = getModelIcon(triggerHint);

    const entries = useMemo(() => {
        const normalized = keyword.trim().toLowerCase();
        if (!normalized) return options;
        return options.filter((option) => option.name.toLowerCase().includes(normalized)
            || option.channel_name.toLowerCase().includes(normalized));
    }, [keyword, options]);

    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger asChild>
                <button
                    type="button"
                    disabled={disabled}
                    className="flex min-w-0 max-w-full items-center gap-2 rounded-xl border border-border bg-muted/30 px-2.5 py-1.5 text-left text-xs transition-colors hover:bg-muted/60 disabled:cursor-not-allowed disabled:opacity-60"
                >
                    <Icon aria-hidden="true" className={iconClassName} width={18} height={18} />
                    <span className="min-w-0 flex-1">
                        <span className="block truncate font-semibold text-foreground">{triggerLabel}</span>
                        <span className="block truncate text-[11px] text-muted-foreground">{triggerHint}</span>
                    </span>
                    {isSubmitting
                        ? <Loader2 className="size-3.5 shrink-0 animate-spin text-muted-foreground" />
                        : <ChevronDown className="size-3.5 shrink-0 text-muted-foreground" />}
                </button>
            </PopoverTrigger>
            <PopoverContent
                align="start"
                side="bottom"
                sideOffset={6}
                className="w-72 rounded-2xl border border-border/60 bg-card p-2 shadow-xl"
            >
                <div className="relative mb-2">
                    <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input
                        value={keyword}
                        onChange={(event) => setKeyword(event.target.value)}
                        placeholder={t('searchPlaceholder')}
                        className="h-8 rounded-xl border-border/60 bg-background/70 pl-7 pr-2 text-xs shadow-none focus-visible:border-border/60 focus-visible:ring-0"
                        aria-label={t('searchPlaceholder')}
                    />
                </div>
                <div className="max-h-72 overflow-y-auto overscroll-contain">
                    {entries.length === 0 ? (
                        <div className="flex h-16 items-center justify-center text-xs text-muted-foreground">
                            {t('noCandidates')}
                        </div>
                    ) : (
                        <div className="flex flex-col gap-1">
                            {entries.map((option) => {
                                const key = memberKey(option);
                                const { Icon: OptionIcon, className: optionIconClassName } = getModelIcon(option.name);
                                const isCurrent = key === currentKey;
                                return (
                                    <button
                                        key={key}
                                        type="button"
                                        aria-pressed={isCurrent}
                                        onClick={() => {
                                            setOpen(false);
                                            if (isCurrent) return;
                                            onSelect(option);
                                        }}
                                        className={cn(
                                            'flex w-full items-center gap-2 rounded-xl px-2 py-1.5 text-left text-xs transition-colors hover:bg-muted/60',
                                            option.enabled === false && 'opacity-50 grayscale',
                                        )}
                                    >
                                        <OptionIcon aria-hidden="true" className={optionIconClassName} width={16} height={16} />
                                        <span className="min-w-0 flex-1">
                                            <span className="block truncate font-medium text-foreground">{option.channel_name}</span>
                                            <span className="block truncate text-[11px] text-muted-foreground">{option.name}</span>
                                        </span>
                                        {isCurrent && <Check className="size-3.5 shrink-0 text-primary" />}
                                    </button>
                                );
                            })}
                        </div>
                    )}
                </div>
            </PopoverContent>
        </Popover>
    );
}

// SessionCard 展示一个会话的状态、当前渠道切换器和归属请求列表。
export const SessionCard = memo(function SessionCard({
    bucket,
    modelChannels,
    expanded,
    highlighted,
    onToggle,
}: {
    bucket: SessionBucket;
    modelChannels: LLMChannel[]; // 所有渠道与模型的候选组合。
    expanded: boolean;
    highlighted: boolean; // highlighted 表示该会话是通知点击后定位过来的。
    onToggle: (key: string, next: boolean) => void;
}) {
    const t = useTranslations('log.session');
    const { session, logs } = bucket;
    const bindSession = useBindSession();
    const unbindSession = useUnbindSession();
    const [now, setNow] = useState(() => Date.now());
    const isPending = session?.status === 'pending';
    const isError = session?.status === 'error';

    // 仅等待选择渠道时走秒级刷新，避免已绑定会话持续触发重渲染。
    useEffect(() => {
        if (!isPending) return;
        const timer = window.setInterval(() => setNow(Date.now()), 1000);
        return () => window.clearInterval(timer);
    }, [isPending]);

    const handleSelect = useCallback((option: LLMChannel) => {
        if (!session) return;
        bindSession.mutate(
            { sessionId: session.id, channelId: option.channel_id, modelName: option.name },
            {
                onSuccess: () => toast.success(t('bindSuccess')),
                onError: (cause: Error) => toast.error(t('bindFailed'), { description: cause.message }),
            }
        );
    }, [bindSession, session, t]);

    const handleUnbind = useCallback(() => {
        if (!session) return;
        unbindSession.mutate({ sessionId: session.id }, {
            onSuccess: () => toast.success(t('unbindSuccess')),
            onError: (cause: Error) => toast.error(t('unbindFailed'), { description: cause.message }),
        });
    }, [session, t, unbindSession]);

    const clientKey = session ? CLIENT_LABEL_KEYS[session.client] : undefined;
    const clientLabel = clientKey ? t(`client.${clientKey}`) : session?.client || t('client.unknown');
    const cachedTokens = session?.usage.prompt_tokens_details?.cached_tokens ?? 0;
    const remainingSeconds = session && isPending
        ? Math.max(0, Math.ceil((Date.parse(session.last_active_at) + PENDING_TIMEOUT_MS - now) / 1000))
        : 0;
    const currentKey = session && session.channel_id !== 0
        ? memberKey({ channel_id: session.channel_id, name: session.model_name })
        : '';

    return (
        <article
            className={cn(
                'flex flex-col rounded-3xl border bg-card text-card-foreground',
                isPending && 'border-amber-500/50 bg-amber-500/5 ring-2 ring-amber-500/30',
                isError && !isPending && 'border-destructive/50 bg-destructive/5',
                !isPending && !isError && 'border-border',
                highlighted && 'ring-2 ring-primary/40',
            )}
        >
            <div className="flex flex-col gap-3 p-4">
                <div className="flex min-w-0 flex-wrap items-center gap-2 text-sm">
                    <span className="min-w-0 flex-1 truncate font-semibold" title={session?.label ?? bucket.label ?? bucket.key}>
                        {session?.label || bucket.label || bucket.key}
                    </span>
                    {session && (
                        <>
                            <Badge variant="secondary" className="shrink-0 px-1.5 py-0 text-[10px]">{clientLabel}</Badge>
                            <Badge variant="outline" className="shrink-0 px-1.5 py-0 text-[10px] text-muted-foreground">{session.requested_model}</Badge>
                            <Badge
                                variant="outline"
                                className={cn(
                                    'shrink-0 px-1.5 py-0 text-[10px] font-medium',
                                    isPending && 'border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400',
                                    isError && 'border-destructive/40 bg-destructive/10 text-destructive',
                                    !isPending && !isError && 'border-primary/30 bg-primary/10 text-primary',
                                )}
                            >
                                {t(`status.${session.status}`)}
                            </Badge>
                        </>
                    )}
                </div>

                {session && (
                    <div className="flex flex-wrap items-center gap-2">
                        <SessionChannelPicker
                            options={modelChannels}
                            currentKey={currentKey}
                            disabled={bindSession.isPending}
                            isSubmitting={bindSession.isPending}
                            triggerLabel={session.channel_name || t('unboundChannel')}
                            triggerHint={session.model_name || t('unboundModel')}
                            onSelect={handleSelect}
                        />
                        {session.channel_id !== 0 && (
                            <button
                                type="button"
                                disabled={unbindSession.isPending}
                                onClick={handleUnbind}
                                className="flex items-center gap-1.5 rounded-lg px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
                            >
                                {unbindSession.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Unlink className="size-3.5" />}
                                {t('unbind')}
                            </button>
                        )}
                    </div>
                )}

                {isPending && (
                    <div className="flex flex-wrap items-center gap-2 rounded-xl border border-amber-500/25 bg-amber-500/10 px-2.5 py-2 text-xs text-amber-700 dark:text-amber-300">
                        <Timer className="size-3.5 shrink-0" />
                        <span className="font-medium">{t('waitingSelection')}</span>
                        <span className="tabular-nums">
                            {remainingSeconds > 0 ? t('countdown', { seconds: remainingSeconds }) : t('timedOut')}
                        </span>
                        {session && session.pending_count > 1 && (
                            <Badge variant="outline" className="border-amber-500/40 px-1.5 py-0 text-[10px]">
                                {t('pendingCount', { count: session.pending_count })}
                            </Badge>
                        )}
                    </div>
                )}

                {isError && session?.error && (
                    <div className="overflow-hidden rounded-xl border border-destructive/20 bg-destructive/10 p-2.5">
                        <p className="flex items-start gap-1.5 text-xs text-destructive">
                            <AlertCircle className="mt-px size-3.5 shrink-0" />
                            <span className="line-clamp-3 whitespace-pre-line">{session.error}</span>
                        </p>
                        <p className="mt-1.5 pl-5 text-[11px] text-muted-foreground">{t('errorRetryHint')}</p>
                    </div>
                )}

                <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-xs tabular-nums text-muted-foreground">
                    <span className="flex items-center gap-1.5">
                        <Hash className="size-3.5 shrink-0 text-blue-500" />
                        {t('requests', { count: session?.request_count ?? logs.length })}
                    </span>
                    <span className="flex items-center gap-1.5">
                        <DollarSign className="size-3.5 shrink-0 text-emerald-500" />
                        <span className="font-medium text-emerald-600 dark:text-emerald-400">{(session?.cost ?? 0).toFixed(6)}</span>
                    </span>
                    <span className="flex items-center gap-1.5">
                        <ArrowDownToLine className="size-3.5 shrink-0 text-green-500" />
                        {((session?.usage.prompt_tokens ?? 0) - cachedTokens).toLocaleString()}
                    </span>
                    <span className="flex items-center gap-1.5">
                        <ArrowUpFromLine className="size-3.5 shrink-0 text-purple-500" />
                        {(session?.usage.completion_tokens ?? 0).toLocaleString()}
                    </span>
                </div>

                <button
                    type="button"
                    onClick={() => onToggle(bucket.key, !expanded)}
                    aria-expanded={expanded}
                    className="flex items-center gap-1.5 self-start rounded-lg px-1.5 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                    <ChevronDown className={cn('size-3.5 transition-transform duration-200', expanded && 'rotate-180')} />
                    {expanded ? t('collapse') : t('expand', { count: logs.length })}
                </button>
            </div>

            {expanded && (
                <div className="flex flex-col gap-3 border-t border-border/60 px-4 py-3">
                    {logs.length === 0 ? (
                        <span className="py-2 text-center text-xs text-muted-foreground">{t('noRequests')}</span>
                    ) : (
                        logs.map((log) => <LogCard key={log.id} log={log} />)
                    )}
                </div>
            )}
        </article>
    );
});
