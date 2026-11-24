concat_file { '/etc/c12.conf': }
concat_fragment { 'plain':  target => '/etc/c12.conf', order => 1, content => "public\n" }
concat_fragment { 'secret': target => '/etc/c12.conf', order => 2, content => Sensitive("s3cret-token\n") }
concat_file { '/etc/c12plain.conf': }
concat_fragment { 'plain2': target => '/etc/c12plain.conf', content => "public\n" }
