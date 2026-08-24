import { ChannelType, type Channel, useFetchModel } from '@/api/channel';
import {
    Select,
    SelectContent,
    SelectItem,
    SelectTrigger,
    SelectValue,
} from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import { toast } from 'sonner';
import { useTranslations } from 'use-intl';
import { useEffect, useState } from 'react';
import { RefreshCw, X, Plus } from 'lucide-react';

export interface ChannelFormData {
    name: string;
    type: ChannelType;
    base_url: string;
    key: string;
    custom_header: Channel['custom_header'];
    header_blocklist: string;
    channel_proxy: string;
    param_override: string;
    model: string;
    custom_model: string;
    enabled: boolean;
    proxy: boolean;
    auto_sync: boolean;
    match_regex: string;
}

interface ChannelFormProps {
    formData: ChannelFormData;
    onFormDataChange: (data: ChannelFormData) => void;
    onSubmit: (event: React.FormEvent<HTMLFormElement>) => void;
    isPending: boolean;
    submitText: string;
    pendingText: string;
    onCancel?: () => void;
    cancelText?: string;
    idPrefix?: string;
}

import {
    Accordion,
    AccordionContent,
    AccordionItem,
    AccordionTrigger,
} from "@/components/ui/accordion";

export function ChannelForm({
    formData,
    onFormDataChange,
    onSubmit,
    isPending,
    submitText,
    pendingText,
    onCancel,
    cancelText,
    idPrefix = 'channel',
}: ChannelFormProps) {
    const t = useTranslations('channel.form');

    // Ensure the form always shows at least one custom Header row.
    useEffect(() => {
        if (!formData.custom_header || formData.custom_header.length === 0) {
            onFormDataChange({ ...formData, custom_header: [{ header_key: '', header_value: '' }] });
        }
    }, [formData, onFormDataChange]);

    const autoModels = formData.model
        ? formData.model.split(',').map((m) => m.trim()).filter(Boolean)
        : [];
    const customModels = formData.custom_model
        ? formData.custom_model.split(',').map((m) => m.trim()).filter(Boolean)
        : [];
    const hasModels = autoModels.length + customModels.length > 0;
    const [inputValue, setInputValue] = useState('');

    const fetchModel = useFetchModel();

    const updateModels = (nextAuto: string[], nextCustom: string[]) => {
        const model = nextAuto.join(',');
        const custom_model = nextCustom.join(',');
        if (formData.model === model && formData.custom_model === custom_model) return;
        onFormDataChange({ ...formData, model, custom_model });
    };

    const handleRefreshModels = () => {
        if (!formData.base_url || !formData.key) return;
        fetchModel.mutate(
            {
                type: formData.type,
                base_url: formData.base_url.trim(),
                key: formData.key.trim(),
                proxy: formData.proxy,
                channel_proxy: formData.channel_proxy?.trim() || null,
                match_regex: formData.match_regex.trim() || null,
                custom_header: formData.custom_header?.filter((h) => h.header_key.trim()) || [],
            },
            {
                onSuccess: (data) => {
                    if (data && data.length > 0) {
                        const nextAuto = Array.from(new Set([...autoModels, ...data].map((m) => m.trim()).filter(Boolean)));
                        updateModels(nextAuto, customModels);
                        toast.success(t('modelRefreshSuccess'));
                    } else {
                        toast.warning(t('modelRefreshEmpty'));
                    }
                },
                onError: (error) => {
                    const errorMessage = error instanceof Error ? error.message : String(error);
                    toast.error(t('modelRefreshFailed'), { description: errorMessage });
                },
            }
        );
    };

    const handleAddModel = (model: string) => {
        const trimmedModel = model.trim();
        if (trimmedModel && !customModels.includes(trimmedModel) && !autoModels.includes(trimmedModel)) {
            updateModels(autoModels, [...customModels, trimmedModel]);
        }
        setInputValue('');
    };

    const handleRemoveAutoModel = (model: string) => {
        updateModels(autoModels.filter(m => m !== model), customModels);
    };

    const handleRemoveCustomModel = (model: string) => {
        updateModels(autoModels, customModels.filter(m => m !== model));
    };

    const handleInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        if (e.key === 'Enter') {
            e.preventDefault();
            if (inputValue.trim()) handleAddModel(inputValue);
        }
    };

    const handleAddHeader = () => {
        onFormDataChange({
            ...formData,
            custom_header: [...(formData.custom_header ?? []), { header_key: '', header_value: '' }],
        });
    };

    const handleUpdateHeader = (idx: number, patch: Partial<Channel['custom_header'][number]>) => {
        const next = (formData.custom_header ?? []).map((h, i) => (i === idx ? { ...h, ...patch } : h));
        onFormDataChange({ ...formData, custom_header: next });
    };

    const handleRemoveHeader = (idx: number) => {
        const curr = formData.custom_header ?? [];
        if (curr.length <= 1) return;
        onFormDataChange({ ...formData, custom_header: curr.filter((_, i) => i !== idx) });
    };

    return (
        <form
            onSubmit={(event) => {
                if (!hasModels) {
                    event.preventDefault();
                    return;
                }
                onSubmit(event);
            }}
            className="space-y-4 px-1"
        >
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-name`} className="text-sm font-medium text-card-foreground">
                        {t('name')}
                    </label>
                    <Input
                        className='rounded-xl'
                        id={`${idPrefix}-name`}
                        type="text"
                        value={formData.name}
                        onChange={(event) => onFormDataChange({ ...formData, name: event.target.value })}
                        required
                    />
                </div>

                <div className="space-y-2">
                    <label htmlFor={`${idPrefix}-type`} className="text-sm font-medium text-card-foreground">
                        {t('type')}
                    </label>
                    <Select
                        value={String(formData.type)}
                        onValueChange={(value) => onFormDataChange({ ...formData, type: value as ChannelType })}
                    >
                        <SelectTrigger id={`${idPrefix}-type`} className="rounded-xl w-full border border-border px-4 py-2 text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
                            <SelectValue />
                        </SelectTrigger>
                        <SelectContent className='rounded-xl'>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIChat)}>{t('typeOpenAIChat')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.OpenAIResponse)}>{t('typeOpenAIResponse')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Anthropic)}>{t('typeAnthropic')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Gemini)}>{t('typeGemini')}</SelectItem>
                            <SelectItem className='rounded-xl' value={String(ChannelType.Volcengine)}>{t('typeVolcengine')}</SelectItem>
                        </SelectContent>
                    </Select>
                </div>
            </div>

            <div className="space-y-2">
                <label htmlFor={`${idPrefix}-base-url`} className="text-sm font-medium text-card-foreground">
                    {t('baseUrl')}
                </label>
                <Input
                    id={`${idPrefix}-base-url`}
                    type="url"
                    value={formData.base_url}
                    onChange={(event) => onFormDataChange({ ...formData, base_url: event.target.value })}
                    placeholder={t('baseUrlUrl')}
                    required
                    className="rounded-xl"
                />
            </div>

            <div className="space-y-2">
                <label htmlFor={`${idPrefix}-key`} className="text-sm font-medium text-card-foreground">
                    {t('apiKey')}
                </label>
                <Input
                    id={`${idPrefix}-key`}
                    type="text"
                    value={formData.key}
                    onChange={(event) => onFormDataChange({ ...formData, key: event.target.value })}
                    placeholder={t('apiKey')}
                    required
                    className="rounded-xl"
                />
            </div>

            <div className="space-y-2">
                <div className="flex items-center justify-between">
                    <label className="text-sm font-medium text-card-foreground">{t('model')}</label>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={handleRefreshModels}
                        disabled={!formData.base_url || !formData.key || fetchModel.isPending}
                        className="h-6 px-2 text-xs text-muted-foreground/50 hover:text-muted-foreground hover:bg-transparent"
                    >
                        <RefreshCw className={`h-3 w-3 mr-1 ${fetchModel.isPending ? 'animate-spin' : ''}`} />
                        {t('modelRefresh')}
                    </Button>
                </div>
                <div className="relative">
                    <Input
                        id={`${idPrefix}-model-custom`}
                        type="text"
                        value={inputValue}
                        onChange={(e) => setInputValue(e.target.value)}
                        onKeyDown={handleInputKeyDown}
                        placeholder={t('modelCustomPlaceholder')}
                        className="pr-10 rounded-xl"
                    />
                    {inputValue.trim() && !customModels.includes(inputValue.trim()) && !autoModels.includes(inputValue.trim()) && (
                        <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={() => handleAddModel(inputValue)}
                            className="absolute rounded-lg right-1 top-1/2 -translate-y-1/2 h-7 w-7 p-0 text-muted-foreground hover:bg-accent hover:text-accent-foreground transition-colors"
                            title={t('modelAdd')}
                        >
                            <Plus className="size-4" />
                        </Button>
                    )}
                </div>

                <div className="space-y-2">
                    <div className="flex items-center justify-between">
                        <label className="text-xs font-medium text-card-foreground">
                            {t('modelSelected')} {(autoModels.length + customModels.length) > 0 && `(${autoModels.length + customModels.length})`}
                        </label>
                        {(autoModels.length + customModels.length) > 0 && (
                            <Button
                                type="button"
                                variant="ghost"
                                size="sm"
                                onClick={() => {
                                    updateModels([], []);
                                }}
                                className="h-6 px-2 text-xs text-muted-foreground/50 hover:text-muted-foreground hover:bg-transparent"
                            >
                                {t('modelClearAll')}
                            </Button>
                        )}
                    </div>
                    <div className="rounded-xl border border-border bg-muted/30 p-2.5 max-h-40 min-h-12 overflow-y-auto">
                        {(autoModels.length + customModels.length) > 0 ? (
                            <div className="flex flex-wrap gap-1.5">
                                {autoModels.map((model) => (
                                    <Badge key={model} variant="secondary" className="bg-muted hover:bg-muted/80">
                                        {model}
                                        <button
                                            type="button"
                                            onClick={() => handleRemoveAutoModel(model)}
                                            className="ml-1 rounded-sm opacity-70 hover:opacity-100 focus:outline-none focus:ring-1 focus:ring-ring"
                                        >
                                            <X className="h-3 w-3" />
                                        </button>
                                    </Badge>
                                ))}
                                {customModels.map((model) => (
                                    <Badge key={model} className="bg-primary hover:bg-primary/90">
                                        {model}
                                        <button
                                            type="button"
                                            onClick={() => handleRemoveCustomModel(model)}
                                            className="ml-1 rounded-sm opacity-70 hover:opacity-100 focus:outline-none focus:ring-1 focus:ring-ring"
                                        >
                                            <X className="h-3 w-3" />
                                        </button>
                                    </Badge>
                                ))}
                            </div>
                        ) : (
                            <div className="flex items-center justify-center h-8 text-xs text-muted-foreground">
                                {t('modelNoSelected')}
                            </div>
                        )}
                    </div>
                </div>
            </div>

            <Accordion type="single" collapsible className="w-full border rounded-xl bg-card">
                <AccordionItem value="advanced" className="border-none">
                    <AccordionTrigger className="text-sm font-medium text-card-foreground py-3 px-4 hover:no-underline hover:bg-muted/30 rounded-xl transition-colors">
                        {t('advanced')}
                    </AccordionTrigger>
                    <AccordionContent className="pt-4 px-4 pb-4 space-y-4 border-t">
                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-channel-proxy`} className="text-sm font-medium text-card-foreground">
                                {t('channelProxy')}
                            </label>
                            <Input
                                id={`${idPrefix}-channel-proxy`}
                                type="text"
                                value={formData.channel_proxy}
                                onChange={(e) => onFormDataChange({ ...formData, channel_proxy: e.target.value })}
                                placeholder={t('channelProxyPlaceholder')}
                                className="rounded-xl"
                            />
                        </div>

                        <div className="space-y-2">
                            <div className="flex items-center justify-between">
                                <label className="text-sm font-medium text-card-foreground">
                                    {t('customHeader')} {formData.custom_header.length > 0 ? `(${formData.custom_header.length})` : ''}
                                </label>
                                <Button
                                    type="button"
                                    variant="ghost"
                                    size="sm"
                                    onClick={handleAddHeader}
                                    className="h-6 px-2 text-xs text-muted-foreground/70 hover:text-muted-foreground hover:bg-transparent"
                                >
                                    <Plus className="h-3 w-3 mr-1" />
                                    {t('customHeaderAdd')}
                                </Button>
                            </div>
                            <div className="space-y-2">
                                {(formData.custom_header ?? []).map((h, idx) => (
                                    <div key={`hdr-${idx}`} className="flex items-center gap-2">
                                        <Input
                                            type="text"
                                            value={h.header_key}
                                            onChange={(e) => handleUpdateHeader(idx, { header_key: e.target.value })}
                                            placeholder={t('customHeaderKey')}
                                            className="rounded-xl flex-1"
                                        />
                                        <Input
                                            type="text"
                                            value={h.header_value}
                                            onChange={(e) => handleUpdateHeader(idx, { header_value: e.target.value })}
                                            placeholder={t('customHeaderValue')}
                                            className="rounded-xl flex-1"
                                        />
                                        <Button
                                            type="button"
                                            variant="ghost"
                                            size="sm"
                                            onClick={() => handleRemoveHeader(idx)}
                                            disabled={(formData.custom_header ?? []).length <= 1}
                                            className="h-8 w-8 p-0 rounded-xl text-muted-foreground hover:text-destructive hover:bg-transparent disabled:opacity-40"
                                            title="Remove"
                                        >
                                            <X className="h-4 w-4" />
                                        </Button>
                                    </div>
                                ))}
                            </div>
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-header-blocklist`} className="text-sm font-medium text-card-foreground">
                                {t('headerBlocklist')}
                            </label>
                            <textarea
                                id={`${idPrefix}-header-blocklist`}
                                value={formData.header_blocklist}
                                onChange={(e) => onFormDataChange({ ...formData, header_blocklist: e.target.value })}
                                placeholder={t('headerBlocklistPlaceholder')}
                                className="min-h-20 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            />
                            <p className="text-xs text-muted-foreground">{t('headerBlocklistHint')}</p>
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-match-regex`} className="text-sm font-medium text-card-foreground">
                                {t('matchRegex')}
                            </label>
                            <Input
                                id={`${idPrefix}-match-regex`}
                                type="text"
                                value={formData.match_regex}
                                onChange={(e) => onFormDataChange({ ...formData, match_regex: e.target.value })}
                                placeholder={t('matchRegexPlaceholder')}
                                className="rounded-xl"
                            />
                        </div>

                        <div className="space-y-2">
                            <label htmlFor={`${idPrefix}-param-override`} className="text-sm font-medium text-card-foreground">
                                {t('paramOverride')}
                            </label>
                            <textarea
                                id={`${idPrefix}-param-override`}
                                value={formData.param_override}
                                onChange={(e) => onFormDataChange({ ...formData, param_override: e.target.value })}
                                placeholder={t('paramOverridePlaceholder')}
                                className="min-h-28 w-full rounded-xl border border-border bg-background px-3 py-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            />
                        </div>
                    </AccordionContent>
                </AccordionItem>
            </Accordion>

            <div className="flex flex-wrap items-center justify-between gap-4 p-4 rounded-xl bg-muted/20 border border-border/50">
                <label className="flex items-center gap-2 cursor-pointer">
                    <Switch
                        checked={formData.enabled}
                        onCheckedChange={(checked) => onFormDataChange({ ...formData, enabled: checked })}
                    />
                    <span className="text-sm font-medium text-card-foreground">{t('enabled')}</span>
                </label>
                <div className="flex items-center gap-6">
                    <label className="flex items-center gap-2 cursor-pointer">
                        <Switch
                            checked={formData.proxy}
                            onCheckedChange={(checked) => onFormDataChange({ ...formData, proxy: checked })}
                        />
                        <span className="text-sm text-card-foreground">{t('proxy')}</span>
                    </label>
                    <label className="flex items-center gap-2 cursor-pointer">
                        <Switch
                            checked={formData.auto_sync}
                            onCheckedChange={(checked) => onFormDataChange({ ...formData, auto_sync: checked })}
                        />
                        <span className="text-sm text-card-foreground">{t('autoSync')}</span>
                    </label>
                </div>
            </div>

            <div className={`flex flex-col gap-3 pt-2 ${onCancel ? 'sm:flex-row' : ''}`}>
                {onCancel && cancelText && (
                    <Button
                        type="button"
                        variant="secondary"
                        onClick={onCancel}
                        className="w-full sm:flex-1 rounded-2xl h-12"
                    >
                        {cancelText}
                    </Button>
                )}
                <Button
                    type="submit"
                    disabled={isPending || !hasModels}
                    className="w-full sm:flex-1 rounded-2xl h-12"
                >
                    {isPending ? pendingText : submitText}
                </Button>
            </div>
        </form>
    );
}
