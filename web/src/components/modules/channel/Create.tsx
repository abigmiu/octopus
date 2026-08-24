import { useState } from 'react';
import {
    MorphingDialogClose,
    MorphingDialogTitle,
    MorphingDialogDescription,
    useMorphingDialog,
} from '@/components/ui/morphing-dialog';
import { useCreateChannel, ChannelType } from '@/api/channel';
import { useTranslations } from 'use-intl';
import { ChannelForm, type ChannelFormData } from './Form';

export function CreateDialogContent() {
    const { setIsOpen } = useMorphingDialog();
    const createChannel = useCreateChannel();
    const [formData, setFormData] = useState<ChannelFormData>({
        name: '',
        type: ChannelType.OpenAIChat,
        base_url: '',
        key: '',
        custom_header: [],
        header_blocklist: '',
        channel_proxy: '',
        param_override: '',
        model: '',
        custom_model: '',
        auto_sync: false,
        enabled: true,
        proxy: false,
        match_regex: '',
    });
    const t = useTranslations('channel.create');

    const handleSubmit = (event: React.FormEvent<HTMLFormElement>) => {
        event.preventDefault();
        const normalizedHeaders = (formData.custom_header ?? [])
            .map((h) => ({ header_key: h.header_key.trim(), header_value: h.header_value }))
            .filter((h) => h.header_key && h.header_value !== '');

        const channelProxy = formData.channel_proxy.trim();
        const paramOverride = formData.param_override.trim();
        createChannel.mutate(
            {
                name: formData.name,
                type: formData.type,
                enabled: formData.enabled,
                base_url: formData.base_url.trim(),
                key: formData.key.trim(),
                model: formData.model,
                custom_model: formData.custom_model,
                proxy: formData.proxy,
                auto_sync: formData.auto_sync,
                custom_header: normalizedHeaders,
                header_blocklist: formData.header_blocklist.trim(),
                channel_proxy: channelProxy,
                param_override: paramOverride,
                match_regex: formData.match_regex.trim(),
            },
            {
                onSuccess: () => {
                    setFormData({
                        name: '',
                        type: ChannelType.OpenAIChat,
                        base_url: '',
                        key: '',
                        custom_header: [],
                        header_blocklist: '',
                        channel_proxy: '',
                        param_override: '',
                        model: '',
                        custom_model: '',
                        auto_sync: false,
                        enabled: true,
                        proxy: false,
                        match_regex: '',
                    });
                    setIsOpen(false);
                }
            });
    };

    return (
        <div className="w-screen max-w-full md:max-w-xl h-full min-h-0 flex flex-col">
            <MorphingDialogTitle className="shrink-0">
                <header className="mb-6 flex items-center justify-between">
                    <h2 className="text-2xl font-bold text-card-foreground">{t('dialogTitle')}</h2>
                    <MorphingDialogClose
                        className="relative right-0 top-0"
                        variants={{
                            initial: { opacity: 0, scale: 0.8 },
                            animate: { opacity: 1, scale: 1 },
                            exit: { opacity: 0, scale: 0.8 }
                        }}
                    />
                </header>
            </MorphingDialogTitle>
            <MorphingDialogDescription disableLayoutAnimation className="flex-1 min-h-0 overflow-auto">
                <ChannelForm
                    formData={formData}
                    onFormDataChange={setFormData}
                    onSubmit={handleSubmit}
                    isPending={createChannel.isPending}
                    submitText={t('submit')}
                    pendingText={t('submitting')}
                    idPrefix="new-channel"
                />
            </MorphingDialogDescription>
        </div>
    );
}
