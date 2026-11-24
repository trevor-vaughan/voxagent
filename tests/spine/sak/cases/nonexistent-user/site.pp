ssh_authorized_key { 'g1': user => 'ghost', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1' }
ssh_authorized_key { 'g2': user => 'ghost', target => '/tmp/g2/authorized_keys', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1' }
