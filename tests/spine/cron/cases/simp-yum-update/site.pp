# simp::yum::schedule with its defaults: yum-cron.erb's command.
cron { 'simp_yum_update':
  ensure   => 'present',
  command  => '/usr/bin/yum clean all  > /dev/null; /usr/bin/yum -q -y update --skip-broken -R 5 > /dev/null',
  user     => 'root',
  minute   => '12',
  hour     => '0',
  monthday => '*',
  month    => '*',
  weekday  => '*',
}
