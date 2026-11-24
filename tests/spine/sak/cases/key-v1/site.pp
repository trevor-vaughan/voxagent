ssh_authorized_key { 'legacy@host': user => 'alice', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1' }
ssh_authorized_key { 'legacy2@host': user => 'alice', ensure => absent }
