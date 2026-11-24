Puppet::Type.type(:vox_value).provide(:file) do
  desc 'Stores the value in a file.'

  def value
    File.exist?(resource[:path]) ? File.read(resource[:path]).chomp : :absent
  end

  def value=(v)
    debug 'vox_value provider wrote the value'
    raise Puppet::Error, "refusing to write #{v}" if v == 'fail'
    # The host must survive module code that prints, and must be killable.
    if v == 'chatty'
      puts 'chatty on $stdout'
      STDOUT.write("chatty on STDOUT\n")
      system('echo chatty from a child')
    end
    Process.kill(:KILL, Process.pid) if v == 'die'

    File.write(resource[:path], "#{v}\n")
  end

  def refresh
    if resource[:value] == 'refusing-refresh'
      resource.notice 'refusing to refresh'
      raise Puppet::Error, 'refresh refused'
    end

    File.write("#{resource[:path]}.refreshed", "refreshed\n", mode: 'a')
  end
end
