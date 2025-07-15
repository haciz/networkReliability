#!/bin/bash

echo "====== Network Monitoring Troubleshooter ======"
echo "Checking Docker status..."

# Check if Docker is running
if ! docker info >/dev/null 2>&1; then
  echo "[ERROR] Docker is not running. Please start Docker and try again."
  exit 1
fi

echo "Docker is running."
echo "Checking for running containers..."

# Check if our containers are running
MONITOR_RUNNING=$(docker ps -q -f name=network-monitor)
PROMETHEUS_RUNNING=$(docker ps -q -f name=prometheus)
GRAFANA_RUNNING=$(docker ps -q -f name=grafana)

if [ -z "$MONITOR_RUNNING" ] || [ -z "$PROMETHEUS_RUNNING" ] || [ -z "$GRAFANA_RUNNING" ]; then
  echo "[WARNING] Not all containers are running."

  if [ -z "$MONITOR_RUNNING" ]; then
    echo "- Monitor container is not running"
  fi

  if [ -z "$PROMETHEUS_RUNNING" ]; then
    echo "- Prometheus container is not running"
  fi

  if [ -z "$GRAFANA_RUNNING" ]; then
    echo "- Grafana container is not running"
  fi

  echo "\nWould you like to restart all containers? (y/n)"
  read -r RESTART

  if [[ "$RESTART" =~ ^[Yy]$ ]]; then
    echo "Stopping all containers..."
    docker-compose down
    echo "Starting all containers..."
    docker-compose up -d
  fi
else
  echo "All containers are running."
fi

# Check container logs for errors
echo "\nChecking container logs for errors..."

echo "\nMonitor container logs:"
docker logs network-monitor --tail 10

echo "\nPrometheus container logs:"
docker logs prometheus --tail 10

echo "\nGrafana container logs:"
docker logs grafana --tail 10

# Check network connectivity
echo "\nChecking network connectivity..."

echo "Checking if monitor metrics endpoint is accessible..."
if curl -s http://localhost:2112/metrics > /dev/null; then
  echo "[SUCCESS] Monitor metrics endpoint is accessible."
else
  echo "[ERROR] Cannot access monitor metrics endpoint at http://localhost:2112/metrics"
fi

echo "Checking if Prometheus is accessible..."
if curl -s http://localhost:9090/-/healthy > /dev/null; then
  echo "[SUCCESS] Prometheus is accessible."
else
  echo "[ERROR] Cannot access Prometheus at http://localhost:9090"
fi

echo "Checking if Grafana is accessible..."
if curl -s http://localhost:3000/api/health > /dev/null; then
  echo "[SUCCESS] Grafana is accessible."
else
  echo "[ERROR] Cannot access Grafana at http://localhost:3000"
fi

echo "\n====== Troubleshooting Advice ======"
echo "1. If containers are not starting, check the Docker logs above for specific errors."
echo "2. Make sure ports 2112, 9090, and 3000 are not already in use by other applications."
echo "3. Ensure your firewall isn't blocking these ports."
echo "4. Try running each service directly to isolate the issue:"
echo "   - Monitor: go run cmd/monitor/main.go"
echo "   - Prometheus: docker run -p 9090:9090 prom/prometheus"
echo "   - Grafana: docker run -p 3000:3000 grafana/grafana"
echo "5. If you've made changes to the configuration files, restart the containers:"
echo "   docker-compose restart"
echo "\nFor accessing the services:"
echo "- Monitor metrics: http://localhost:2112/metrics"
echo "- Prometheus: http://localhost:9090"
echo "- Grafana: http://localhost:3000 (login with admin/admin)"
