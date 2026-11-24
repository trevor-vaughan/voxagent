# A Tier 3 test type whose generate produces a notify when `make` is true.
Puppet::Type.newtype(:vox_gen) do
  @doc = 'voxagent extension-host generate test type.'
  newparam(:name, namevar: true)
  newparam(:make) { defaultto(false) }

  def generate
    raise Puppet::Error, 'generate broke' if self[:make].to_s == 'raise'
    if self[:make].to_s == 'before'
      self[:before] = Puppet::Resource.new('class', 'main')
      return []
    end
    return [] unless self[:make].to_s == 'true'

    [Puppet::Type.type(:notify).new(name: "made-by-#{self[:name]}")]
  end
end
