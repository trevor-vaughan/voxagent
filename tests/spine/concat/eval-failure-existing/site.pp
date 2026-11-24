# M19's shape without `source` (a fragment with `source` is delegated): the
# eval_generate fails, on M3c's incomparable numeric order keys, while the
# target exists with other content, mode and owner.
concat_file { '/etc/cx1.conf': mode => '0600', owner => 'daemon' }
concat_fragment { 'a': target => '/etc/cx1.conf', order => 10, content => "a\n" }
concat_fragment { '5': target => '/etc/cx1.conf', order => 10, content => "5\n" }
