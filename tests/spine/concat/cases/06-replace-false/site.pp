concat_file { '/etc/c06exist.conf': replace => false }
concat_file { '/etc/c06absent.conf': replace => false }
concat_file { '/etc/c06true.conf': replace => true }
concat_fragment { 'f-exist':  target => '/etc/c06exist.conf',  content => "new\n" }
concat_fragment { 'f-absent': target => '/etc/c06absent.conf', content => "new\n" }
concat_fragment { 'f-true':   target => '/etc/c06true.conf',   content => "new\n" }
