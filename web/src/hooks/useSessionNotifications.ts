import { useCallback, useEffect, useRef } from 'react';
import { toast } from 'sonner';
import { useTranslations } from 'use-intl';
import { useSessions, type SessionOverview } from '@/api/session';
import { useAppStore } from '@/stores/app';
import { useSessionViewStore } from '@/stores/session';

// notificationSupported 表示当前环境是否可用 Notification API。
function notificationSupported() {
    return typeof window !== 'undefined' && 'Notification' in window;
}

// requestNotificationPermission 在权限尚未决定时申请通知权限。
export async function requestNotificationPermission() {
    if (!notificationSupported()) return 'denied' as NotificationPermission;
    if (Notification.permission !== 'default') return Notification.permission;
    try {
        return await Notification.requestPermission();
    } catch {
        return 'denied' as NotificationPermission;
    }
}

// useSessionNotifications 订阅共享会话流，在出现新的等待选择会话时提醒管理员。
export function useSessionNotifications() {
    const t = useTranslations('log.notification');
    const { sessions } = useSessions();
    const enabled = useSessionViewStore((state) => state.notificationEnabled);
    const focusSession = useSessionViewStore((state) => state.focusSession);
    const setCurrentPage = useAppStore((state) => state.setCurrentPage);
    const notifiedRef = useRef(new Set<string>()); // 已提醒过的等待会话，离开等待状态后移除以便再次提醒。

    // 通知权限只能在用户手势中申请，因此等首次交互再询问一次。
    useEffect(() => {
        if (!enabled || !notificationSupported() || Notification.permission !== 'default') return;

        const handleGesture = () => {
            void requestNotificationPermission();
        };
        window.addEventListener('pointerdown', handleGesture, { once: true });
        window.addEventListener('keydown', handleGesture, { once: true });

        return () => {
            window.removeEventListener('pointerdown', handleGesture);
            window.removeEventListener('keydown', handleGesture);
        };
    }, [enabled]);

    const openSession = useCallback((id: string) => {
        setCurrentPage('log');
        focusSession(id);
    }, [focusSession, setCurrentPage]);

    // notify 优先使用浏览器通知，权限被拒或不可用时降级为页面内提示。
    const notify = useCallback((session: SessionOverview) => {
        const title = t('title');
        const body = session.label || session.id;

        if (enabled && notificationSupported() && Notification.permission === 'granted') {
            try {
                const notification = new Notification(title, {
                    body,
                    tag: `octopus-session-${session.id}`,
                    icon: '/logo.svg',
                });
                notification.onclick = () => {
                    window.focus();
                    notification.close();
                    openSession(session.id);
                };
                return;
            } catch {
                // 构造通知失败时继续走页面内提示。
            }
        }

        toast.warning(title, {
            description: body,
            action: { label: t('action'), onClick: () => openSession(session.id) },
        });
    }, [enabled, openSession, t]);

    useEffect(() => {
        const notified = notifiedRef.current;
        const pendingIds = new Set<string>();
        for (const session of sessions) {
            if (session.status !== 'pending') continue;
            pendingIds.add(session.id);
            if (notified.has(session.id)) continue;
            notified.add(session.id);
            notify(session);
        }
        for (const id of notified) {
            if (!pendingIds.has(id)) notified.delete(id);
        }
    }, [notify, sessions]);
}
