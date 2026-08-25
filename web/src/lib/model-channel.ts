import type { LLMChannel } from '@/api/model';

// modelChannelKey 生成「渠道 + 模型」组合的稳定标识。
export function modelChannelKey(channelId: number, modelName: string) {
    return `${channelId}-${modelName}`;
}

// memberKey 由渠道模型关联数据生成组合标识。
export function memberKey(member: Pick<LLMChannel, 'channel_id' | 'name'>) {
    return modelChannelKey(member.channel_id, member.name);
}
