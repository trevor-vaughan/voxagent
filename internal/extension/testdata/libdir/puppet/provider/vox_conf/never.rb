Puppet::Type.type(:vox_conf).provide(:never) do
  desc 'Never suitable.'
  confine false: true

  def value
    :absent
  end

  def value=(_v); end
end
