ssh_authorized_key { 'alice-other@x': user => 'alice', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQDKtwEp2Qo0Jn6yS9X0q8yTzrBH0mUL2v8c9aF3dQ2k', options => ['no-pty'] }
ssh_authorized_key { 'bobs@x': user => 'bob', type => 'ssh-rsa', key => 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl' }
ssh_authorized_key { 'shared@x': user => 'bob', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1' }
