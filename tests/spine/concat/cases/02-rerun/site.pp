concat_file { '/etc/c01.conf': }
concat_fragment { 'c': target => '/etc/c01.conf', content => "three\n" }
concat_fragment { 'a': target => '/etc/c01.conf', content => "one\n" }
concat_fragment { 'b': target => '/etc/c01.conf', content => "two\n" }
