# A catalog File, by title and by path, is never tidied.
file { '/srv/me/by-title': ensure => file, content => "new\n" }
file { 'by-path': path => '/srv/me/by-path', ensure => file }
tidy { '/srv/me': recurse => true }
