user { 'erin': ensure => present, managehome => true }
ssh_authorized_key { 'erin@x': user => 'erin', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1' }
ssh_authorized_key { 'erin@y': user => 'erin', type => 'ssh-rsa', key => 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' } -> User['erin']
