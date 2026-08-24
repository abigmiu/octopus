import { create } from 'zustand';
import { createJSONStorage, persist } from 'zustand/middleware';

interface SessionViewState {
    notificationEnabled: boolean; // 是否允许在会话等待选择渠道时弹出浏览器通知。
    focusSessionId: string | null; // 日志页待定位并展开的会话 ID。
    setNotificationEnabled: (value: boolean) => void;
    focusSession: (id: string) => void;
    clearFocusSession: () => void;
}

// useSessionViewStore 在通知层和日志页之间传递通知开关与定位目标。
export const useSessionViewStore = create<SessionViewState>()(
    persist(
        (set) => ({
            notificationEnabled: true,
            focusSessionId: null,
            setNotificationEnabled: (value) => set({ notificationEnabled: value }),
            focusSession: (id) => set({ focusSessionId: id }),
            clearFocusSession: () => set({ focusSessionId: null }),
        }),
        {
            name: 'session-view-options-storage',
            storage: createJSONStorage(() => localStorage),
            partialize: (state) => ({ notificationEnabled: state.notificationEnabled }),
        }
    )
);
