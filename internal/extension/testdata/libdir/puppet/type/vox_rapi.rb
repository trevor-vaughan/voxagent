require 'puppet/resource_api'

Puppet::ResourceApi.register_type(
  name: 'vox_rapi',
  docs: 'voxagent extension-host Resource API test type.',
  attributes: { name: { type: 'String', behaviour: :namevar, desc: 'The name.' } }
)
