Puppet::Type.type(:vox_marker).provide(:ruby) do
  desc 'Manages the marker with Ruby File calls.'

  def exists?
    File.exist?(resource[:path])
  end

  def create
    File.write(resource[:path], '')
  end

  def destroy
    File.delete(resource[:path])
  end
end
