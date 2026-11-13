package bridge

import "testing"

func TestParseSysIgnoresServiceAndStaleShape(t *testing.T) {
	id, event, ok := ParseSys(
		"$SYS/brokers/emqx@127.0.0.1/clients/94a990cb56a8/connected",
		[]byte(`{"username":"root","clientid":"94a990cb56a8","connected_at":1762070400000,"ts":1762070400000}`),
	)
	if !ok || id != "94a990cb56a8" || event.Kind != "connected" || event.ConnectedAt != 1762070400000 {
		t.Fatalf("event = %+v %v", event, ok)
	}
	if _, _, ok := ParseSys(
		"$SYS/brokers/emqx@127.0.0.1/clients/iot-dev-service-a/connected",
		[]byte(`{"username":"iot-dev-service","clientid":"iot-dev-service-a","connected_at":10}`),
	); ok {
		t.Fatal("service event accepted")
	}
}
