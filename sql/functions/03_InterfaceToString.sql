CREATE OR REPLACE FUNCTION InterfaceToString AS (Sampler, Interface) ->
    if(dictGetOrNull('InterfaceNames', 'Description', (IPv6NumToString(Sampler), Interface)) IS NULL,
       concat(SamplerToString(Sampler), ' - ', toString(Interface)),
       concat(SamplerToString(Sampler), ' - ', toString(Interface), ' [',
              dictGetString('InterfaceNames', 'Description', (IPv6NumToString(Sampler), Interface)), ']'));
