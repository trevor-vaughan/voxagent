ini_setting { 's': path => '/etc/m.ini', section => 'a', setting => 'k', value => Sensitive('s3cret') }
ini_subsetting { 'ss': path => '/etc/m.ini', section => 'a', setting => 'o', subsetting => 'pw', value => Sensitive('s3cret') }
