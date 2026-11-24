# frozen_string_literal: true

# voxagent's extension host.
#
# voxagent embeds this file, writes it into a fresh 0700 directory, checks its
# SHA-256 against the value compiled into the binary, and runs it as
#
#   ruby host.rb <state-dir>
#
# It evaluates the resources of legacy (Tier 3) types with Puppet's own
# ResourceHarness, one request at a time, and answers in the framed JSON
# protocol of internal/extension. voxagent keeps the graph, ordering, refresh
# propagation and the report. See docs/maintainers/extension-host.md.
#
# Citations are path:line against Puppet 8.10.0's lib/puppet/.

require 'json'
require 'set'

module VoxagentHost
  PROTOCOL = 1
  MAX_FRAME = 64 * 1024 * 1024

  # Log lines Puppet emits while one request is served. Registered as a
  # Puppet::Util::Log destination; the harness never writes to a report itself
  # (configurer.rb:308 is how an agent's report receives logs).
  class Sink
    def initialize
      @lines = []
    end

    def <<(msg)
      @lines << msg
    end

    def drain
      out = @lines
      @lines = []
      out
    end
  end

  # The puppetdb terminus's coercion, as tests/spine/lib/puppet/reports/gatejson.rb
  # reproduces it: voxagent's report must carry what PuppetDB would receive, and
  # the protocol accepts only valid UTF-8.
  def self.utf8(str)
    copy = str.dup.force_encoding('UTF-8')
    return copy.encode('UTF-8') if copy.valid_encoding?

    copy.force_encoding('US-ASCII').encode('UTF-8', invalid: :replace, undef: :replace, replace: "�")
  end

  def self.clean(value)
    case value
    when String then utf8(value)
    when Symbol then utf8(value.to_s)
    when Array then value.map { |e| clean(e) }
    when Hash then value.to_h { |k, v| [clean(k), clean(v)] }
    else value
    end
  end

  # An event as the puppetdb terminus serialises it: the strings it sends are
  # coerced to UTF-8 first, as tests/spine/lib/puppet/reports/gatejson.rb:23-104
  # reproduces the terminus, since Event#to_data_hash writes an ASCII-8BIT
  # String in Base64 (pops/serialization/to_stringified_converter.rb:97-103). A Binary parameter
  # and any value a provider reads as bytes are such Strings. The event is
  # given new values; the Strings and collections it shares with its resource
  # are left untouched.
  def self.event_hash(event)
    %i[@status @property @desired_value @previous_value @message].each do |ivar|
      v = event.instance_variable_get(ivar)
      case v
      when String then event.instance_variable_set(ivar, utf8(v))
      when Array then event.instance_variable_set(ivar, v.map { |e| e.is_a?(String) ? utf8(e) : e })
      when Set then event.instance_variable_set(ivar, Set.new(v.map { |e| e.is_a?(String) ? utf8(e) : e }))
      end
    end
    clean(event.to_data_hash)
  end

  class Server
    CONTAINERS = %w[class stage node whit component].freeze
    LOG_LEVELS = %w[notice info debug].freeze

    def initialize(input, output, state_dir)
      @in = input
      @out = output
      @state_dir = state_dir
      @sink = Sink.new
      @post_eval = []
      @providerless = []
    end

    def run
      write('hello' => { 'protocol' => PROTOCOL, 'puppet_version' => Puppet.version, 'ruby_version' => RUBY_VERSION })
      loop do
        request = read
        return if request.nil?

        op, arg = request.first
        reply =
          begin
            result = dispatch(op, arg)
            # load and a failed refresh split their lines and supply their own 'logs'.
            { op => result.key?('logs') ? result : result.merge('logs' => logs) }
          rescue StandardError, ScriptError => e
            @sink.drain
            { 'error' => { 'message' => VoxagentHost.utf8("#{e.class}: #{e.message}") } }
          end
        write(reply)
        return if op == 'shutdown'
      end
    end

    private

    def dispatch(op, arg)
      case op
      when 'load' then load_catalog(arg)
      when 'relationships' then relationships(arg)
      when 'evaluate' then evaluate(arg)
      when 'skip' then skip(arg)
      when 'eval_generate' then eval_generate(arg)
      when 'refresh' then refresh(arg)
      when 'overly_deferred' then overly_deferred(arg)
      when 'finish' then finish
      when 'shutdown' then {}
      else raise ArgumentError, "unknown operation #{op.inspect}"
      end
    end

    def read
      head = @in.read(4)
      return nil if head.nil?
      raise IOError, 'truncated frame header' unless head.bytesize == 4

      len = head.unpack1('N')
      raise IOError, "frame of #{len} bytes exceeds the 64 MiB limit" if len > MAX_FRAME

      body = @in.read(len)
      raise IOError, 'truncated frame' unless body && body.bytesize == len

      request = JSON.parse(body.force_encoding('UTF-8'))
      raise IOError, 'a request is an object with exactly one key' unless request.is_a?(Hash) && request.size == 1

      request
    end

    def write(obj)
      body = JSON.generate(obj).b
      raise IOError, "reply of #{body.bytesize} bytes exceeds the 64 MiB limit" if body.bytesize > MAX_FRAME

      @out.write([body.bytesize].pack('N'), body)
      @out.flush
    end

    def logs
      @sink.drain.map do |m|
        line = { 'level' => m.level.to_s, 'source' => VoxagentHost.utf8(m.source.to_s),
                 'message' => VoxagentHost.utf8(m.message.to_s) }
        line['file'] = VoxagentHost.utf8(m.file.to_s) if m.file
        line['line'] = m.line.to_i if m.line
        line
      end
    end

    def resource(ref)
      @by_ref.fetch(ref) { raise ArgumentError, "#{ref} is not in the loaded catalog" }
    end

    # load: Puppet's settings for an agent run against libdir, then the catalog
    # converted as `puppet apply --catalog` converts it (application/apply.rb:352-359).
    def load_catalog(arg)
      raise ArgumentError, 'load may be called once' if @catalog

      libdir = arg.fetch('libdir')
      vardir = arg.fetch('vardir')
      data = arg.fetch('catalog')
      log_level = arg.fetch('log_level')
      unless LOG_LEVELS.include?(log_level)
        raise ArgumentError, "log_level must be one of #{LOG_LEVELS.join(', ')}, got #{log_level.inspect}"
      end
      # libdir is put on $LOAD_PATH only here (puppet.rb:163-165). confdir and
      # codedir are run-private so nothing writes to the owner's configuration.
      # vardir is voxagent's own, as an agent run shares one: what a Tier 3
      # type's code keeps under Puppet[:vardir] (reboot_notify's
      # reboot_notifications.json, a filebucket it backs up into) lands where
      # the agent's does. Puppet::Util::Storage is never stored there:
      # Catalog#apply (which the Configurer calls) loads and stores state.yaml
      # (resource/catalog.rb:234,248), and the host calls neither it nor
      # transaction.evaluate.
      Puppet.initialize_settings(['--confdir', File.join(@state_dir, 'conf'),
                                  '--vardir', vardir,
                                  '--codedir', File.join(@state_dir, 'code'),
                                  '--libdir', libdir])
      # The run's level: Puppet drops a line below it before the sink sees it, so
      # a provider's `debug` reaches the report only under --debug.
      Puppet::Util::Log.level = log_level.to_sym
      Puppet::Util::Log.newdesttype :voxagent_sink do
        match 'VoxagentHost::Sink'
        def initialize(sink)
          @sink = sink
        end

        def handle(msg)
          @sink << msg
        end
      end
      Puppet::Util::Log.newdestination(@sink)
      # An empty modulepath: modules on the node's disk must not shadow libdir
      # types (puppet.rb:214-236, util/autoload.rb:164-165).
      env = Puppet::Node::Environment.create((data['environment'] || 'production').to_sym, [])
      Puppet.push_context({ current_environment: env, loaders: Puppet::Pops::Loaders.new(env) },
                          'voxagent extension host')

      types = classify(data.fetch('resources'), libdir)
      catalog = Puppet::Resource::Catalog.from_data_hash(data)
      # puppet apply's pass between reading a catalog and converting it
      # (application/apply.rb:357): each Binary becomes its bytes and a
      # top-level Sensitive is unwrapped into sensitive_parameters
      # (pops/evaluator/deferred_resolver.rb:87-118, :154-172). Every other rich
      # value stays Puppet's own object. Pre-flight refuses a Deferred on any
      # non-container resource, so the resolver calls no function and needs no
      # facts; measured at about 0.2 ms.
      Puppet::Pops::Evaluator::DeferredResolver.resolve_and_replace(nil, catalog, env, false)
      invalid = convert(catalog, data.fetch('resources'))
      @tx = Puppet::Transaction.new(@catalog, nil, Puppet::Graph::SequentialPrioritizer.new)
      # Transaction.new builds an unloaded Persistence (transaction.rb:46): its
      # nil system values keep corrective_change false (D-6). A nil persistence
      # would fail every resource (transaction/resource_harness.rb:15).
      @harness = Puppet::Transaction::ResourceHarness.new(@tx)

      load_logs = logs # what loading logged; generate's lines are collected apart
      generation = generate_all(types, libdir)
      generate_logs = logs
      # Puppet canonicalises a container's title (Class[main] is Class[Main],
      # resource.rb:581-588), which is no override. Containers are what
      # classify skips; a generated resource is named by its own ref.
      containers = data.fetch('resources').select do |d|
        CONTAINERS.include?(d.fetch('type').downcase) || (d['kind'] && d['kind'] != 'compilable_type')
      end.to_h { |d| ["#{d['type']}[#{d['title']}]", true] }
      resources = {}
      @by_ref.each do |ref, r|
        used = r.respond_to?(:provider) && r.provider ? r.provider.class.name.to_s : nil
        info = { 'provider_used' => used }
        aliases = aliases_of(r)
        info['aliases'] = aliases unless aliases.empty?
        retitle(info, ref, r) unless containers.key?(ref)
        resources[ref] = info
      end
      # Puppet's catalog order after the generation pass: add_resource put each
      # generated resource next to its parent (additional_resource_generator.rb:36-50,
      # :137-145, resource/catalog.rb:99-124), before it for a depth-first type.
      order = @catalog.resources.map { |r| @ref_of.fetch(r) }
      { 'types' => types, 'metaparams' => Puppet::Type.metaparams.map(&:to_s).sort,
        'invalid' => invalid, 'generated' => generation, 'resources' => resources, 'order' => order,
        'logs' => load_logs,
        'generate_logs' => generate_logs, 'failed_to_generate' => @generator.resources_failed_to_generate }
    end

    # Classifies every non-container type before conversion, because to_ral
    # silently turns an unknown type without kind into a component
    # (resource.rb:518-524). Containers are what ral.IsContainerResource says
    # they are: the container types, and any kind but compilable_type, which
    # covers classes and defined-type instances.
    def classify(resources, libdir)
      prefix = File.join(File.expand_path(libdir), '')
      typed = resources.reject { |r| r['kind'] && r['kind'] != 'compilable_type' }
      typed.map { |r| r.fetch('type').downcase }.uniq.sort.each_with_object({}) do |name, out|
        next if CONTAINERS.include?(name)

        klass = Puppet::Type.type(name) # nil when no type loads (metatype/manager.rb:155-182)
        if klass.nil?
          out[name] = { 'class' => 'missing' }
          next
        end
        loaded = Puppet::Util::Autoload.loaded["puppet/type/#{name}"]
        out[name] = {
          # register_type defines type_definition (puppet-resource_api lib/puppet/resource_api.rb:39-60).
          'class' => klass.respond_to?(:type_definition) ? 'tier2' : 'tier3',
          'origin' => loaded && File.expand_path(loaded[0]).start_with?(prefix) ? 'libdir' : 'core',
          'refreshable' => klass.method_defined?(:refresh),
          'eval_generates' => klass.method_defined?(:eval_generate),
          # newtype(:x, self_refresh: true) (type.rb:2186-2189): event_manager.rb:79
          # queues the resource's own events back to it.
          'self_refresh' => klass.self_refresh ? true : false
        }
      end
    end

    # Catalog#to_catalog (resource/catalog.rb:592-647), resource by resource:
    # to_ral stops at the first error, and voxagent reports all of them.
    #
    # Resources are named by the catalog's own refs, which is how voxagent names
    # them. Puppet's can differ: it canonicalises a class title when it reads
    # one (resource.rb:581-588), so a compiled catalog's Class[main] is
    # Class[Main] here.
    def convert(catalog, resources)
      original = resources.to_h { |r| [Puppet::Resource.from_data_hash(r).ref, "#{r['type']}[#{r['title']}]"] }
      @catalog = Puppet::Resource::Catalog.new(catalog.name, catalog.environment_instance)
      @catalog.version = catalog.version
      @catalog.catalog_format = catalog.catalog_format
      @by_ref = {}
      @ref_of = {}.compare_by_identity
      invalid = []
      catalog.resources.each do |res|
        ref = original.fetch(res.ref)
        copy = res.copy_as_resource
        copy.catalog = @catalog
        ral = copy.to_ral
        @catalog.add_resource(ral)
        @by_ref[ref] = ral
        @ref_of[ral] = ref
      rescue StandardError => e
        invalid << { 'ref' => ref, 'message' => VoxagentHost.utf8(e.message) }
      end
      catalog.edges.each do |edge|
        source = @by_ref[original[edge.source.ref]]
        target = @by_ref[original[edge.target.ref]]
        @catalog.add_edge(source, target, edge.label) if source && target
      end
      invalid
    end

    # Puppet's Transaction#evaluate (transaction.rb:99-100)
    # runs generate on every resource, through the generator's own code: it
    # rescues and logs, recurses, tags, contains and orders what it makes
    # (transaction/additional_resource_generator.rb:21-53, :131-211).
    def generate_all(types, libdir)
      @generator = Puppet::Transaction::AdditionalResourceGenerator.new(@catalog, nil, Puppet::Graph::SequentialPrioritizer.new)
      # add_resource (additional_resource_generator.rb:137-148) is the one place
      # Puppet pairs a new resource with the resource that generated it, for
      # generate and eval_generate alike, and it keeps no record of the pair.
      generators = @generators = {}.compare_by_identity
      @generator.singleton_class.prepend(Module.new do
        define_method(:add_resource) do |res, parent_resource, priority = nil|
          generators[res] ||= parent_resource
          super(res, parent_resource, priority)
        end
      end)
      changes = observe { @catalog.vertices.dup.each { |r| @generator.generate_additional_resources(r) } }
      changes['resources'].each do |made|
        name = made['type'].downcase
        types[name] ||= classify([{ 'type' => made['type'] }], libdir)[name]
      end
      changes
    end

    # eval_generate (additional_resource_generator.rb:56-75), at the resource's
    # turn in voxagent's walk. New resources are added to the catalog as
    # add_resources adds them; voxagent applies them as the parent's unit.
    def eval_generate(arg)
      r = resource(arg.fetch('ref'))
      # additional_resource_generator.rb:56-57: a type without eval_generate generates nothing.
      unless r.respond_to?(:eval_generate)
        return { 'resources' => [], 'containers' => {}, 'generators' => {}, 'before' => [], 'overrides' => [],
                 'existing' => [], 'failed' => false }
      end

      existing = []
      failed = false
      made = []
      changes = observe do
        made = begin
          Array(r.eval_generate).map { |g| @catalog.resource(g.ref) || g }
        rescue StandardError => e
          failed = true
          r.log_exception(e, "Failed to generate additional resources using 'eval_generate': #{e}")
          []
        end
        existing = made.select { |g| @ref_of.key?(g) }.map { |g| @ref_of[g] }
        # add_resource (additional_resource_generator.rb:131-148) with no
        # priority: voxagent's walk, not a relationship graph, orders them.
        made.reject { |g| @ref_of.key?(g) }.each { |g| @generator.send(:add_resource, g, r) }
      end
      # Reported in the order eval_generate returned them, which is the order
      # add_resources applies them in (additional_resource_generator.rb:131-135,
      # SequentialPrioritizer's increasing priorities); catalog order is the
      # reverse, because each is added after the generator.
      order = made.each_with_index.to_h { |g, i| [@ref_of[g], i] }
      changes['resources'] = changes['resources'].sort_by.with_index do |res, i|
        [order.fetch("#{res['type']}[#{res['title']}]", made.size), i]
      end
      changes.merge('existing' => existing, 'failed' => failed)
    end

    # Runs the block and reports what it did to the catalog: resources it
    # added (registered under Puppet's ref), each one's container, `before`
    # values it appended (add_generated_directed_dependency), and parameters
    # it changed on resources that already existed (the overrides).
    def observe
      before = snapshot
      yield
      made = @catalog.resources.reject { |r| @ref_of.key?(r) }
      made.each do |r|
        ref = generated_ref(r)
        @by_ref[ref] = r
        @ref_of[r] = ref
      end
      out = { 'resources' => made.map { |r| serialise(r) }, 'containers' => {}, 'generators' => {}, 'before' => [],
              'overrides' => [] }
      made.each do |r|
        container = @catalog.container_of(r)
        out['containers'][@ref_of[r]] = @ref_of[container] if container && @ref_of[container]
        generator = @generators[r]
        out['generators'][@ref_of[r]] = @ref_of[generator] if generator && @ref_of[generator]
      end
      @by_ref.each do |ref, r|
        was = before[ref]
        next unless was

        now = params_of(r)
        added = Array(now['before']) - Array(was['before'])
        added.each { |target| out['before'] << { 'source' => ref, 'target' => target } }
        changed = now.reject { |k, v| k == 'before' || was[k] == v }
        next if changed.empty?

        sensitive = changed.keys.select { |k| r.parameters[k.to_sym]&.sensitive }
        out['overrides'] << { 'ref' => ref, 'parameters' => changed, 'sensitive' => sensitive }
      end
      # Relationship values appended to a new resource are already in its
      # serialised parameters only if set at creation; report them as edges too.
      made.each do |r|
        orig = Array(r.original_parameters[:before]).map { |v| v.respond_to?(:ref) ? v.ref : v.to_s }
        (Array(params_of(r)['before']) - orig).each { |target| out['before'] << { 'source' => @ref_of[r], 'target' => target } }
      end
      out
    end

    def snapshot
      @by_ref.transform_values { |r| params_of(r) }
    end

    # A property's should value, except file's content, whose should is the
    # checksum while the bytes are actual_content (type/file/content.rb).
    def params_of(r)
      r.parameters.to_h do |k, p|
        [k.to_s, value_of(p.respond_to?(:actual_content) && p.actual_content ? p.actual_content : r[k])]
      end
    end

    # A resource voxagent knows is named by the catalog's own spelling (Class[main],
    # where Puppet says Class[Main]); any other by Puppet's ref.
    def value_of(v)
      case v
      when Array then v.map { |e| value_of(e) }
      when Puppet::Type then @ref_of[v] || v.ref
      when Puppet::Resource
        known = @catalog.resource(v.ref)
        known && @ref_of[known] || v.ref
      else VoxagentHost.clean(v)
      end
    end

    # A generated resource in catalog JSON shape (resource.rb to_data_hash),
    # built from what the generator passed, as Puppet itself does at
    # additional_resource_generator.rb:195-207; to_resource would query the system.
    # Tags keep the TagSet's insertion order, which the report writes
    # (resource/status.rb:172,216).
    def serialise(r)
      res = Puppet::Resource.new(r.class, r.title, parameters: r.original_parameters)
      data = res.to_data_hash
      { 'type' => data['type'], 'title' => data['title'], 'tags' => r.tags.to_a,
        'exported' => false, 'kind' => 'compilable_type',
        'parameters' => (data['parameters'] || {}).transform_values { |v| value_of(v) } }
    end

    # The ref a generated resource is named by wherever the host names it: the
    # type and title serialise writes (resource.rb to_data_hash), which is how
    # voxagent spells its ref. Type#ref (type.rb:2527) spells it the same for
    # every type newtype can define, as a '::' type name is no Ruby constant
    # (util/classgen.rb:159); this keeps the host to one spelling.
    def generated_ref(r)
      res = Puppet::Resource.new(r.class, r.title)
      "#{res.type}[#{res.title}]"
    end

    # catalog.rb:184-219: a resource titled other than its name is also known
    # by its name.
    # catalog.rb:184-219 stores [class_name, *key]; only a single-key alias
    # can be written as a reference. A compiled catalog's Class[main] is
    # Class[Main] to Puppet (resource.rb:581-588) and aliased by its original
    # title: that alias is the resource's own ref in the catalog's spelling,
    # which is no alias to voxagent.
    def aliases_of(r)
      own = @ref_of[r]
      Array(@catalog.instance_variable_get(:@aliases)[r.ref]).select { |k| k.size == 2 }
                                                            .map { |c, key| "#{c}[#{key}]" }.reject { |a| a == own }
    end

    # A type that overrides title (kernel_parameter.rb:42-48) names r by a ref
    # other than the catalog's, and the catalog then cannot find r by its
    # catalog title (resource/catalog.rb:378-395).
    def retitle(info, ref, r)
      type, rest = ref.split('[', 2)
      title = rest.delete_suffix(']')
      if r.title.to_s != title
        info['puppet_ref'] = VoxagentHost.utf8(r.ref)
        info['puppet_title'] = VoxagentHost.utf8(r.title.to_s)
      end
      info['unresolvable'] = true unless @catalog.resource(type, title).equal?(r)
    end

    # The four auto-relation blocks (type.rb:2000-2018) through autorelation
    # (type.rb:2076-2113), which keeps only refs the catalog holds.
    def relationships(arg)
      relations = {}
      arg.fetch('refs').each do |ref|
        r = resource(ref)
        relations[ref] = {
          'require' => r.autorequire(@catalog).map { |e| @ref_of.fetch(e.source) },
          'before' => r.autobefore(@catalog).map { |e| @ref_of.fetch(e.target) },
          'subscribe' => r.autosubscribe(@catalog).map { |e| @ref_of.fetch(e.source) },
          'notify' => r.autonotify(@catalog).map { |e| @ref_of.fetch(e.target) }
        }
      end
      { 'relations' => relations }
    end

    def evaluate(arg)
      r = resource(arg.fetch('ref'))
      # noop per call; the noop metaparameter would still win (type.rb:1132), and
      # voxagent refuses it at pre-flight.
      Puppet[:noop] = arg.fetch('noop') == true
      return unsuitable unless r.suitable?

      if prefetch_failed?(r)
        status = Puppet::Resource::Status.new(r)
        status.fail_with_event(prefetch_message(r)) # transaction.rb:337
        status.skipped = true
        r.warning('Skipping because provider prefetch failed') # transaction.rb:427
      else
        status = @harness.evaluate(r) # transaction/resource_harness.rb:18
      end
      # Event#to_data_hash drops invalidate_refreshes, which event_manager.rb:82
      # reads, so it travels beside the events.
      { 'events' => status.events.map { |e| VoxagentHost.event_hash(e) },
        'failed' => status.failed ? true : false, 'skipped' => status.skipped ? true : false,
        'invalidate_refreshes' => status.events.any?(&:invalidate_refreshes) }
    end

    # A resource skipped for failed dependencies still runs pre_process: the
    # prefetch, and the failure event propagate_failure adds when it failed
    # (transaction.rb:111-121, :333-339). voxagent logs the skip itself.
    def skip(arg)
      r = resource(arg.fetch('ref'))
      return unsuitable unless r.suitable?

      events = []
      if prefetch_failed?(r)
        status = Puppet::Resource::Status.new(r)
        status.fail_with_event(prefetch_message(r))
        events = status.events.map { |e| VoxagentHost.event_hash(e) }
      end
      { 'events' => events, 'failed' => !events.empty?, 'skipped' => true, 'invalidate_refreshes' => false }
    end

    # relationship_graph.rb:116-137 defers a resource that is not suitable?
    # (type.rb:1923-1939) and never pre-processes or evaluates it, whatever its
    # dependencies did. voxagent's walk defers it too, and calls
    # overly_deferred when a drain cannot place it.
    def unsuitable
      { 'events' => [], 'failed' => true, 'skipped' => false, 'unsuitable' => true, 'invalidate_refreshes' => false }
    end

    # The overly-deferred handler (transaction.rb:124-136): a chosen provider
    # is named at once; a type with no provider at all is logged once, at
    # teardown (finish). voxagent records the failed status itself.
    def overly_deferred(arg)
      r = resource(arg.fetch('ref'))
      if r.provider
        r.err format('Provider %<name>s is not functional on this host', name: r.provider.class.name)
      else
        @providerless << r.type unless @providerless.include?(r.type)
      end
      {}
    end

    # pre_process (transaction.rb:111-121): remember the provider for
    # post_resource_eval, then prefetch lazily, once per provider class
    # (transaction.rb:243-260), logging a failure as transaction.rb:384 does.
    def prefetch_failed?(r)
      klass = r.provider.class
      @post_eval << klass if klass.respond_to?(:post_resource_eval) && !@post_eval.include?(klass)
      @tx.prefetch_if_necessary(r)
      @tx.prefetch_failed_providers[r.type][klass.name] ? true : false
    end

    def prefetch_message(r)
      format("Prefetch failed for %<type>s provider '%<name>s'", type: r.type, name: r.provider.class.name)
    end

    # event_manager.rb:144-165: voxagent writes the Triggered and Failed lines
    # and events; the host runs the callback and Puppet's log_exception.
    # Puppet logs "Failed to call" between the callback's own lines and
    # log_exception's, so they are returned apart for voxagent to interleave.
    def refresh(arg)
      r = resource(arg.fetch('ref'))
      begin
        r.refresh
        { 'refreshed' => true, 'error' => nil, 'exception_logs' => [] }
      rescue StandardError => e
        callback = logs
        r.log_exception(e)
        { 'refreshed' => false, 'error' => VoxagentHost.utf8(e.to_s), 'logs' => callback, 'exception_logs' => logs }
      end
    end

    # transaction.rb:143-153.
    def finish
      @providerless.each do |type|
        Puppet.err format('Could not find a suitable provider for %<type>s', type: type)
      end
      @post_eval.each do |provider|
        provider.post_resource_eval
      rescue StandardError => e
        Puppet.log_exception(e, format('post_resource_eval failed for provider %<p>s', p: provider))
      end
      {}
    end
  end
end

# The protocol keeps private duplicates of stdin and stdout. Ruby opens every
# descriptor close-on-exec, so no command Puppet runs inherits them; fd 1 then
# points at stderr and fd 0 at the null device, so module code that prints or
# reads cannot corrupt the framing. ARGV is consumed so Kernel#gets in module
# code does not read the state directory's path.
protocol_out = $stdout.dup.binmode
protocol_in = $stdin.dup.binmode
$stdout.reopen($stderr)
$stdin.reopen(File::NULL)
state_dir = ARGV.shift
raise ArgumentError, 'usage: host.rb <state-dir>' unless state_dir

require 'puppet'
VoxagentHost::Server.new(protocol_in, protocol_out, state_dir).run
exit 0
