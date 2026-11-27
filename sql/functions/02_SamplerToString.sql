CREATE OR REPLACE FUNCTION SamplerToString AS (Sampler) ->
    coalesce(dictGet('SamplerConfig', 'Description', IPv6NumToString(Sampler)), IPv6ToString(Sampler));
