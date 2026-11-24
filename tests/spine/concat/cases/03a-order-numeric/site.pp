concat_file { '/etc/c03.conf': order => numeric }
concat_fragment { 'o90':   target => '/etc/c03.conf', order => 90,   content => "90\n" }
concat_fragment { 'o100':  target => '/etc/c03.conf', order => 100,  content => "100\n" }
concat_fragment { 'o1000': target => '/etc/c03.conf', order => 1000, content => "1000\n" }
concat_fragment { 'on':    target => '/etc/c03.conf', order => '10___n', content => "10___n\n" }
concat_fragment { 'str9':  target => '/etc/c03.conf', order => '9', content => "str9\n" }
concat_fragment { 'tb':    target => '/etc/c03.conf', order => 5, content => "5-tb\n" }
concat_fragment { 'ta':    target => '/etc/c03.conf', order => 5, content => "5-ta\n" }
concat_fragment { '2':     target => '/etc/c03.conf', order => 7, content => "7-name2\n" }
concat_fragment { '10':    target => '/etc/c03.conf', order => 7, content => "7-name10\n" }
