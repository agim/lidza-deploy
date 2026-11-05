require "bundler/setup"
require "rails"
require "action_controller/railtie"

class Fixture < Rails::Application
  config.load_defaults 8.1
  config.eager_load = true
  config.secret_key_base = ENV.fetch("SECRET_KEY_BASE")
  config.hosts = ["rails-fixture.example.com"]
  config.logger = Logger.new($stdout)
  routes.append do
    get "/up", to: proc { |_env| [ENV["FAIL_READINESS"] == "1" ? 503 : 200, {"content-type" => "text/plain"}, ["ready"]] }
    get "/persist", to: proc { |_env|
      files = ["storage/proof", "public/system/proof", "public/uploads/proof"]
      previous = File.exist?(files.first) ? File.read(files.first).to_i : 0
      raise "persistent paths diverged" if previous > 0 && files.any? { |file| !File.exist?(file) || File.read(file).to_i != previous }
      count = previous + 1
      files.each { |file| File.write(file, count.to_s) }
      [200, {"content-type" => "text/plain"}, [count.to_s]]
    }
  end
end
