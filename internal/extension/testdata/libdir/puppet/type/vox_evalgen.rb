# A Tier 3 test type whose eval_generate makes new resources at its turn:
# `make` notifies named after the resource, or raises when `make` is 'raise'.
Puppet::Type.newtype(:vox_evalgen) do
  @doc = 'voxagent extension-host eval_generate test type.'
  newparam(:name, namevar: true)
  newparam(:make) { defaultto('1') }

  def eval_generate
    raise Puppet::Error, 'eval_generate broke' if self[:make] == 'raise'

    (1..self[:make].to_i).map { |i| Puppet::Type.type(:notify).new(name: "#{self[:name]}-child-#{i}") }
  end
end
