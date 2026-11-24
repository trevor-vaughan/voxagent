Puppet::Type.type(:vox_prefetch).provide(:good) do
  desc 'Prefetches successfully.'
  def self.prefetch(resources)
    resources.each_value { |r| r.provider = new(name: r[:name], ensure: :absent) }
  end

  def self.post_resource_eval
    Puppet.notice('vox_prefetch good post_resource_eval ran')
  end

  def ensure
    @property_hash[:ensure] || :absent
  end

  def create
    @property_hash[:ensure] = :present
  end
end
