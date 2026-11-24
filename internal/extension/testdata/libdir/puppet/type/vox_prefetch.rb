# A Tier 3 test type whose providers prefetch: `good` succeeds, `bad` raises,
# and `good` logs from post_resource_eval.
Puppet::Type.newtype(:vox_prefetch) do
  @doc = 'voxagent extension-host prefetch test type.'
  newparam(:name, namevar: true)
  newproperty(:ensure) do
    newvalue(:present) { provider.create }
  end
end
