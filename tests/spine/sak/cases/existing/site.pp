ssh_authorized_key { 'alice@laptop': user => 'alice', type => 'ssh-rsa', key => 'AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajDw4o6gJy8UtmsBJKXe7JvPJ5mVGrTxQ1w8aXx1', options => ['no-pty', 'command="x y"'] }
