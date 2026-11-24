# A Tier 3 test type for voxagent's extension host: one property backed by a
# file, a refresh that appends to a log file, and all four auto-relations.
Puppet::Type.newtype(:vox_value) do
  @doc = 'voxagent extension-host test type.'

  newparam(:name, namevar: true)
  newparam(:path) { desc 'The file holding the value.' }
  newparam(:peer) { desc 'A vox_value this one autonotifies.' }

  newproperty(:value) do
    desc 'The value; "fail" makes the setter raise.'
  end

  autorequire(:file) { [File.dirname(self[:path])] if self[:path] }
  autobefore(:notify) { ["after-#{self[:name]}"] }
  autosubscribe(:notify) { ["before-#{self[:name]}"] }
  autonotify(:vox_value) { [self[:peer]] if self[:peer] }

  def refresh
    provider.refresh
  end
end
