class unstable {
  @@notify { 'exported-thing': message => 'crosses PuppetDB' }
  $stamp = generate('/bin/date')
}
