Puppet::Type.type(:vox_selfref).provide(:ruby) do
  desc 'Always out of sync: value and other read old.'

  def exists?
    resource[:exists_now].to_s == 'true'
  end

  def create; end

  def destroy; end

  def value
    'old'
  end

  def value=(v); end

  def other
    'old'
  end

  def other=(_v)
    raise 'set failed' if resource[:fail_set].to_s == 'true'
  end
end
