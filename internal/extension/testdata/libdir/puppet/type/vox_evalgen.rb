# A Tier 3 test type whose eval_generate makes new resources at its turn:
# `make` notifies named after the resource, or raises when `make` is 'raise'.
# `existing`, a reference to a catalog resource, is returned beside them, as
# concat_file returns its File: Puppet's completion whit then waits for it and
# refreshes the generator's subscribers when it changes.
Puppet::Type.newtype(:vox_evalgen) do
  @doc = 'voxagent extension-host eval_generate test type.'
  newparam(:name, namevar: true)
  newparam(:make) { defaultto('1') }
  newparam(:existing)

  def eval_generate
    raise Puppet::Error, 'eval_generate broke' if self[:make] == 'raise'

    made = (1..self[:make].to_i).map { |i| Puppet::Type.type(:notify).new(name: "#{self[:name]}-child-#{i}") }
    made << catalog.resource(self[:existing]) if self[:existing]
    made
  end
end
