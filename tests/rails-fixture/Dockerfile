FROM ruby:3.4.5
WORKDIR /rails
COPY Gemfile /rails/Gemfile
RUN bundle install --jobs 4
COPY . /rails
RUN chmod 755 bin/rails bin/docker-entrypoint && mkdir -p tmp log storage public/system public/uploads && chown -R 1000:1000 /rails
USER 1000:1000
ENTRYPOINT ["/rails/bin/docker-entrypoint"]
CMD ["./bin/rails", "server"]
