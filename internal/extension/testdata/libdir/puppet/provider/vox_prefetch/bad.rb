Puppet::Type.type(:vox_prefetch).provide(:bad) do
  desc 'Prefetch raises.'
  def self.prefetch(_resources)
    raise Puppet::Error, 'bad prefetch'
  end

  def ensure
    :absent
  end

  def create; end
end
