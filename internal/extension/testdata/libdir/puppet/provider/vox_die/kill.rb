Puppet::Type.type(:vox_die).provide(:kill) do
  desc 'Kills the process evaluating it.'

  def value
    :absent
  end

  def value=(_v)
    Process.kill(:KILL, Process.pid)
  end
end
