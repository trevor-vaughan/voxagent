notify { 'before-everything': }
concat_file { '/etc/c13.conf': notify => Exec['notified'] }
concat_fragment { 'f': target => '/etc/c13.conf', content => "new\n" }
exec { 'notified':   command => '/bin/sh -c "echo n >> /etc/c13.notified"', refreshonly => true }
exec { 'subscribed': command => '/bin/sh -c "echo s >> /etc/c13.subscribed"', refreshonly => true, subscribe => Concat_file['/etc/c13.conf'] }
exec { 'required':   command => '/bin/sh -c "echo r >> /etc/c13.required"', require => Concat_file['/etc/c13.conf'] }
notify { 'after-everything': require => Exec['required'] }
notify { 'unrelated': }
