# A Tier 3 test type with no suitable provider: its only provider, `never`, is
# confined away. A resource that names no provider has none; one that names
# `never` has an unsuitable one. Puppet defers both (relationship_graph.rb:116-137).
Puppet::Type.newtype(:vox_conf) do
  @doc = 'voxagent extension-host unsuitable-provider test type.'
  newparam(:name, namevar: true)
  newproperty(:value)
end
