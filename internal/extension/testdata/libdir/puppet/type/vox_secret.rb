# A test type for voxagent's extension host that echoes its parameters in
# every phase, as provider code does when it interpolates resource[:x]:
#  - token and raw are refused while munged, echoing the value as Puppet's group
#    gid does (Invalid GID) and logging "Reraising" at debug (parameter.rb:446).
#    Only the value 'fine' is accepted.
#  - echo and extra are never refused; state's sync, generate and the provider's
#    prefetch print echo; refresh raises with it.
Puppet::Type.newtype(:vox_secret) do
  @doc = 'voxagent extension-host test type that echoes its parameters.'
  newparam(:name, namevar: true)
  newparam(:token) do
    munge { |value| raise Puppet::Error, "Invalid token #{Array(value).join(' / ')}" unless value == 'fine' }
  end
  newparam(:raw) do
    munge { |value| raise Puppet::Error, "Invalid raw #{value}".b + "\xFF".b }
  end
  newparam(:echo)
  newparam(:extra)
  newproperty(:state) do
    def retrieve = 'before'
    def sync = raise(Puppet::Error, "cannot set state for #{resource[:echo]}")
  end

  def refresh = raise(Puppet::Error, "refresh broke for #{self[:echo]}")

  def generate
    Puppet.notice("generating for #{self[:echo]}") if self[:echo]
    []
  end
end
