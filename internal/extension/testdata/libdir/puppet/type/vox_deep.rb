# A Tier 3 test type whose generate is depth-first, as tidy's is
# (type/tidy.rb:236): Puppet inserts what it makes before it in the catalog
# (additional_resource_generator.rb:36-50, resource/catalog.rb:99-110).
Puppet::Type.newtype(:vox_deep) do
  @doc = 'voxagent extension-host test type: depth-first generate.'
  newparam(:name, namevar: true)

  def depthfirst?
    true
  end

  def generate
    [1, 2].map { |i| Puppet::Type.type(:notify).new(name: "#{self[:name]}-#{i}") }
  end
end
