# A Tier 3 test type that refreshes itself after its own events
# (self_refresh, type.rb:2186-2189; event_manager.rb:79), as mount_core's
# mount does.
# Its refresh appends to a log under Puppet[:vardir], which is voxagent's
# --vardir: the log proves the host shares it.
Puppet::Type.newtype(:vox_selfref, self_refresh: true) do
  @doc = 'voxagent extension-host test type: refreshes itself.'
  ensurable
  newparam(:name, namevar: true)
  newparam(:exists_now) { defaultto :true }
  newparam(:fail_refresh) { defaultto :false }
  newparam(:fail_set) { defaultto :false }
  newproperty(:value)
  newproperty(:other)

  def refresh
    File.write(File.join(Puppet[:vardir], 'vox_selfref.log'), "#{self[:name]}\n", mode: 'a')
    raise Puppet::Error, "boom from #{self[:name]}" if self[:fail_refresh].to_s == 'true'
  end
end
