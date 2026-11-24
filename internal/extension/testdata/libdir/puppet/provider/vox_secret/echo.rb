Puppet::Type.type(:vox_secret).provide(:echo) do
  desc 'Prints every resource of the provider it prefetches, to the log and to stderr; fails when one has state "break".'
  def self.prefetch(resources)
    echoes = resources.values.map { |r| r[:echo] }.join(' ')
    Puppet.notice("prefetching #{echoes}")
    # Straight to the host's stderr, which no log relays or redacts.
    $stderr.puts("prefetch stderr: #{echoes}")
    raise Puppet::Error, "prefetch broke for #{echoes}" if resources.values.any? { |r| r[:state] == 'break' }
  end
end
