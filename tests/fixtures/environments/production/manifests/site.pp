$vals = { 'b' => 2, 'a' => 1 }

define fixture::thing (Integer $order) {
  notify { "thing-${title}": message => "${title}/${order}" }
}

class fixture::base {
  $vals.each |$k, $v| { fixture::thing { $k: order => $v } }
  file { '/tmp/fixture-one': ensure => file, content => 'one' }
  file { '/tmp/fixture-two': ensure => file, content => 'two', require => File['/tmp/fixture-one'] }
}

node default {
  include fixture::base
}
