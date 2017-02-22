service exporter
================

Introduction
------------

The code in this repository implements a Prometheus exporter which monitors the
status of processes ran by Upstart on Linux.  Other init systems might be
supported in the future, but the support for any is not currently under
development.

How to build
------------

Having installed a reasonably modern version of Go, run: `go get
github.com/trustly/prometheus_service_exporter`.  This should produce a binary
under `$GOPATH/bin`.

Configuration
-------------

The binary expects at least two command line arguments:

  1. The port to listen on.  All interfaces are currently always listened on.
  This argument is required because no port has been allocated for service
  exporter's use.
  2. The name of the service to monitor, e.g. "cron".

These two required arguments can be followed up with more service names.

Service exporter runs "service foo status" during normal operation to figure
out the status and PID of the process started by each service.  On some
operating systems (such as Ubuntu) this requires the package "dbus" to be
installed.
