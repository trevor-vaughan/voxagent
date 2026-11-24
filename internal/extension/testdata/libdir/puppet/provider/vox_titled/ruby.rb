Puppet::Type.type(:vox_titled).provide(:ruby) do
  desc 'value reads old; fail_set makes the setter raise.'

  def value
    'old'
  end

  def value=(_v)
    raise 'set failed' if resource[:fail_set].to_s == 'true'
  end
end
