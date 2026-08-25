import { lazy, Suspense, useDeferredValue, useEffect, useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { AnimatePresence, motion } from 'motion/react';
import { useAuth } from '@/api/user';
import {
    apiKeyDashboardStatsQueryOptions,
    apiKeyListQueryOptions,
    channelListQueryOptions,
    modelChannelListQueryOptions,
    modelListQueryOptions,
    statsDailyQueryOptions,
    statsHourlyQueryOptions,
    statsTotalQueryOptions,
} from '@/api/queries';
import { AppShell } from '@/components/app-shell';
import { LoginForm } from '@/components/modules/login';
import { APIKeyDashboard } from '@/components/modules/apikey-dashboard';
import { useAppStore } from '@/stores/app';
import { pageImports } from '@/lib/page-preload';

// 页面和顶栏操作共用 pageImports 中的懒加载模块。
const Home = lazy(() => pageImports.home().then((module) => ({ default: module.Home })));
const Channel = lazy(() => pageImports.channel().then((module) => ({ default: module.Channel })));
const Model = lazy(() => pageImports.model().then((module) => ({ default: module.Model })));
const Log = lazy(() => pageImports.log().then((module) => ({ default: module.Log })));
const Setting = lazy(() => pageImports.setting().then((module) => ({ default: module.Setting })));
const ChannelActions = lazy(() => pageImports.channel().then((module) => ({ default: module.ChannelActions })));
const ModelActions = lazy(() => pageImports.model().then((module) => ({ default: module.ModelActions })));

// InitialLoadingGate 在当前界面提交后淡出并移除 HTML 首屏加载动画。
function InitialLoadingGate({ children }: { children: ReactNode }) {
    useEffect(() => {
        const loader = document.getElementById('initial-loader');
        if (!loader || loader.dataset.state === 'hidden') return;

        loader.dataset.state = 'hidden';
        loader.classList.add('octo-hide');
        window.setTimeout(() => loader.remove(), 220);
    }, []);

    return children;
}

// AppContainer 根据认证状态渲染登录页、API Key 页面或普通用户应用。
export function AppContainer() {
    const { isAuthenticated, isAPIKeyAuth, isLoading: authLoading } = useAuth();
    const queryClient = useQueryClient();
    const [apiReady, setAPIReady] = useState(false); // apiReady 表示当前认证模式所需的初始 API 已加载完成。
    const currentPage = useAppStore((state) => state.currentPage);
    // visibleItem 延迟提交页面切换，等待 lazy 模块在 Suspense 中准备完成。
    const visibleItem = useDeferredValue(currentPage);

    useEffect(() => {
        if (authLoading || !isAuthenticated) {
            setAPIReady(false);
            return;
        }

        let cancelled = false;
        setAPIReady(false);

        const requests = isAPIKeyAuth
            ? [
                queryClient.fetchQuery(apiKeyDashboardStatsQueryOptions),
            ]
            : [
                queryClient.fetchQuery(apiKeyListQueryOptions),
                queryClient.fetchQuery(channelListQueryOptions),
                queryClient.fetchQuery(modelListQueryOptions),
                queryClient.fetchQuery(modelChannelListQueryOptions),
                queryClient.fetchQuery(statsDailyQueryOptions),
                queryClient.fetchQuery(statsHourlyQueryOptions),
                queryClient.fetchQuery(statsTotalQueryOptions),
            ];

        void Promise.all(requests).then(() => {
            if (!cancelled) setAPIReady(true);
        }, () => {
            // 初始请求失败后仍进入应用，由各查询页面展示具体错误状态。
            if (!cancelled) setAPIReady(true);
        });

        return () => {
            cancelled = true;
        };
    }, [authLoading, isAPIKeyAuth, isAuthenticated, queryClient]);

    if (authLoading) return null;

    // 登录页面
    if (!isAuthenticated) {
        return (
            <InitialLoadingGate>
                <LoginForm />
            </InitialLoadingGate>
        );
    }

    if (!apiReady) return null;

    // API Key 认证模式 - 显示 API Key Dashboard
    if (isAPIKeyAuth) {
        return (
            <InitialLoadingGate>
                <APIKeyDashboard />
            </InitialLoadingGate>
        );
    }

    // 普通用户应用
    return (
        <AppShell
            actions={
                <Suspense fallback={null}>
                    {visibleItem === 'channel' && <ChannelActions />}
                    {visibleItem === 'model' && <ModelActions />}
                </Suspense>
            }
        >
            <Suspense fallback={null}>
                <InitialLoadingGate>
                    <AnimatePresence mode="sync">
                        <motion.div
                            key={visibleItem}
                            initial={{ opacity: 0, scale: 0.8 }}
                            animate={{
                                opacity: 1,
                                scale: 1,
                                transition: {
                                    duration: 0.5,
                                    ease: [0.16, 1, 0.3, 1],
                                    delay: 0.1,
                                },
                            }}
                            exit={{
                                opacity: 0,
                                scale: 0.98,
                                transition: { duration: 0.25 },
                            }}
                            className="absolute inset-0 min-h-0 overflow-hidden"
                        >
                            {visibleItem === 'home' && <Home />}
                            {visibleItem === 'channel' && <Channel />}
                            {visibleItem === 'model' && <Model />}
                            {visibleItem === 'log' && <Log />}
                            {visibleItem === 'setting' && <Setting />}
                        </motion.div>
                    </AnimatePresence>
                </InitialLoadingGate>
            </Suspense>
        </AppShell>
    );
}
