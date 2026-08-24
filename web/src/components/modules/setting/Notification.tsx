import { useEffect, useState } from 'react';
import { Bell, BellOff } from 'lucide-react';
import { useTranslations } from 'use-intl';
import { toast } from 'sonner';
import { Switch } from '@/components/ui/switch';
import { requestNotificationPermission } from '@/hooks/useSessionNotifications';
import { useSessionViewStore } from '@/stores/session';

// SettingNotification 控制会话等待选择渠道时的浏览器通知开关。
export function SettingNotification() {
    const t = useTranslations('setting');
    const enabled = useSessionViewStore((state) => state.notificationEnabled);
    const setEnabled = useSessionViewStore((state) => state.setNotificationEnabled);
    const [permission, setPermission] = useState<NotificationPermission | 'unsupported'>(
        () => (typeof window !== 'undefined' && 'Notification' in window ? Notification.permission : 'unsupported')
    );

    // 页面回到前台时同步一次权限状态，避免展示过期结果。
    useEffect(() => {
        const sync = () => {
            if (typeof window === 'undefined' || !('Notification' in window)) return;
            setPermission(Notification.permission);
        };
        window.addEventListener('focus', sync);
        return () => window.removeEventListener('focus', sync);
    }, []);

    const handleToggle = async (checked: boolean) => {
        setEnabled(checked);
        if (!checked) return;
        const next = await requestNotificationPermission();
        setPermission(next);
        if (next !== 'granted') toast.warning(t('notification.denied'));
    };

    const statusText = permission === 'unsupported'
        ? t('notification.unsupported')
        : permission === 'granted'
            ? t('notification.granted')
            : permission === 'denied'
                ? t('notification.denied')
                : t('notification.default');

    return (
        <div className="space-y-5 rounded-3xl border border-border bg-card p-6">
            <h2 className="flex items-center gap-2 text-lg font-bold text-card-foreground">
                <Bell className="size-5" />
                {t('notification.title')}
            </h2>
            <div className="flex items-center justify-between gap-4">
                <div className="flex min-w-0 items-center gap-3">
                    {enabled ? <Bell className="size-5 shrink-0 text-muted-foreground" /> : <BellOff className="size-5 shrink-0 text-muted-foreground" />}
                    <div className="min-w-0">
                        <p className="text-sm font-medium">{t('notification.pending.label')}</p>
                        <p className="text-xs text-muted-foreground">{statusText}</p>
                    </div>
                </div>
                <Switch
                    checked={enabled}
                    disabled={permission === 'unsupported'}
                    onCheckedChange={(checked) => void handleToggle(checked)}
                />
            </div>
            <p className="text-xs text-muted-foreground">{t('notification.pending.hint')}</p>
        </div>
    );
}
