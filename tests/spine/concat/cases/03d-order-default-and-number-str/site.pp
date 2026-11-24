# Default order is the string '10'; compare with integer 10 and 9 / 009.
concat_file { '/etc/c03d.conf': }
concat_fragment { 'd':    target => '/etc/c03d.conf', content => "default10\n" }
concat_fragment { 'z009': target => '/etc/c03d.conf', order => '009', content => "009\n" }
concat_fragment { 'i10':  target => '/etc/c03d.conf', order => 10, content => "int10\n" }
