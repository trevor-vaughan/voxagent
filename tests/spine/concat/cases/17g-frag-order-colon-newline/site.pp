concat_file { '/etc/c17.conf': }
concat_fragment { 'x': target => '/etc/c17.conf', order => "a:b", content => 'x' }
concat_fragment { 'y': target => '/etc/c17.conf', order => "a\nb", content => 'x' }
