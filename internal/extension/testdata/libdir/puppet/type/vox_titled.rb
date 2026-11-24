# A Tier 3 test type whose title is not the catalog title, as
# augeasproviders_grub's kernel_parameter (kernel_parameter.rb:42-68): name
# and bootmode are namevars and title is "name:bootmode".
Puppet::Type.newtype(:vox_titled) do
  @doc = 'voxagent extension-host test type: overrides title.'
  newparam(:name) { isnamevar }
  newparam(:bootmode) do
    isnamevar
    newvalues :all, :default, :normal, :recovery
    defaultto :all
  end
  newparam(:fail_set) { defaultto :false }
  newproperty(:value)

  def name
    mode = self[:bootmode].to_s == 'normal' ? 'default' : self[:bootmode]
    "#{self[:name]}:#{mode}"
  end
  alias_method :title, :name

  def refresh; end

  def self.title_patterns
    [[%r{\A([^:]+)\z}, [[:name]]], [%r{^([^:]+):([^:]+)$}, [[:name], [:bootmode]]]]
  end
end
