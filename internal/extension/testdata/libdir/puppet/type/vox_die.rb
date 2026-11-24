# A Tier 3 test type whose provider kills the extension host when its value is
# set, so voxagent's handling of a host lost mid-run is tested end to end.
Puppet::Type.newtype(:vox_die) do
  @doc = 'voxagent extension-host test type that kills its host.'
  newparam(:name, namevar: true)
  newproperty(:value)
end
