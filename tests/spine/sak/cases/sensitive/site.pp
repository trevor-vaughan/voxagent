ssh_authorized_key { 'old@x': user => 'alice', type => 'ssh-rsa', key => Sensitive('AAAAB3NzaC1yc2EAAAADAQABAAABAQDKtwEp2Qo0Jn6yS9X0q8yTzrBH0mUL2v8c9aF3dQ2k') }
ssh_authorized_key { 'new@x': user => 'alice', type => 'ssh-rsa', key => Sensitive('AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl') }
