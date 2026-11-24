ssh_authorized_key { 'alice@laptop': user => 'alice', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1' }
ssh_authorized_key { 'alice@desktop': user => 'alice', type => 'ssh-ed25519', key => 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl', options => ['no-pty', 'from="10.0.0.1"'] }
