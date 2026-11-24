# Legacy (non-resource-API) type: exists so pluginsync has a lib/ file to ship.
Puppet::Type.newtype(:vox_marker) do
  @doc = 'Creates an empty marker file at the given path.'
  ensurable
  newparam(:path, namevar: true) do
    desc 'Absolute path of the marker file.'
  end
end
