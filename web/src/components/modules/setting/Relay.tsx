import { useEffect, useRef, useState } from 'react';
import { useTranslations } from 'use-intl';
import { Forward, Repeat, Timer, Hourglass, RefreshCw } from 'lucide-react';
import { toast } from 'sonner';
import { Input } from '@/components/ui/input';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useSettingList, useSetSetting, SettingKey } from '@/api/setting';

// RELAY_FIELDS 定义转发配置卡片的四个正整数配置项及默认值。
const RELAY_FIELDS = [
    { key: SettingKey.RelayMaxAttempts, labelKey: 'maxAttempts', Icon: Repeat, fallback: '2' },
    { key: SettingKey.RelayRetryIntervalSeconds, labelKey: 'retryInterval', Icon: RefreshCw, fallback: '3' },
    { key: SettingKey.RelayResponseTimeoutSeconds, labelKey: 'responseTimeout', Icon: Timer, fallback: '120' },
    { key: SettingKey.RelayStreamFirstEventTimeoutSeconds, labelKey: 'streamFirstEventTimeout', Icon: Hourglass, fallback: '30' },
] as const;

// SettingRelay 渲染全局转发重试与超时配置。
export function SettingRelay() {
    const t = useTranslations('setting');
    const { data: settings } = useSettingList();
    const setSetting = useSetSetting();

    const [values, setValues] = useState<Record<string, string>>({});
    const initialValues = useRef<Record<string, string>>({});

    // 设置列表返回后同步一次输入框，未配置的键使用默认值展示。
    useEffect(() => {
        if (!settings) return;
        const next: Record<string, string> = {};
        RELAY_FIELDS.forEach((field) => {
            next[field.key] = settings.find((item) => item.key === field.key)?.value ?? field.fallback;
        });
        initialValues.current = next;
        queueMicrotask(() => setValues(next));
    }, [settings]);

    // handleSave 仅在值发生变化且为正整数时提交。
    const handleSave = (key: string) => {
        const value = (values[key] ?? '').trim();
        if (value === initialValues.current[key]) return;
        if (!/^[1-9]\d*$/.test(value)) {
            setValues((current) => ({ ...current, [key]: initialValues.current[key] ?? '' }));
            toast.error(t('relay.invalid'));
            return;
        }
        setSetting.mutate({ key, value }, {
            onSuccess: () => {
                initialValues.current = { ...initialValues.current, [key]: value };
                toast.success(t('saved'));
            },
        });
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                <Forward className="h-5 w-5" />
                {t('relay.title')}
            </h2>

            {RELAY_FIELDS.map((field) => (
                <div key={field.key} className="flex items-center justify-between gap-4">
                    <div className="flex items-center gap-3">
                        <field.Icon className="h-5 w-5 text-muted-foreground" />
                        <Tooltip>
                            <TooltipTrigger asChild>
                                <span className="text-sm font-medium cursor-help">{t(`relay.${field.labelKey}.label`)}</span>
                            </TooltipTrigger>
                            <TooltipContent side="top" sideOffset={10} align="center">
                                {t(`relay.${field.labelKey}.hint`)}
                            </TooltipContent>
                        </Tooltip>
                    </div>
                    <Input
                        type="number"
                        min={1}
                        step={1}
                        value={values[field.key] ?? ''}
                        onChange={(e) => setValues((current) => ({ ...current, [field.key]: e.target.value }))}
                        onBlur={() => handleSave(field.key)}
                        placeholder={field.fallback}
                        className="w-48 rounded-xl"
                    />
                </div>
            ))}
        </div>
    );
}
