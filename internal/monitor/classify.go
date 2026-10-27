package monitor

// Category is what a sensor measures or detects, derived from Home Assistant
// metadata only (domain and device_class), never from the entity ID text.
type Category string

const (
	CategoryTemperature Category = "temperature"
	CategoryHumidity    Category = "humidity"
	CategoryBattery     Category = "battery"     // numeric charge level
	CategoryBatteryLow  Category = "battery_low" // binary: on means low
	CategoryMotion      Category = "motion"
	CategoryDoor        Category = "door"
	CategoryWindow      Category = "window"
	CategoryLeak        Category = "leak" // device_class moisture
)

// Domains NeptHass monitors.
const (
	DomainSensor       = "sensor"
	DomainBinarySensor = "binary_sensor"
)

type classKey struct{ domain, deviceClass string }

// classes is the whole classification table. Extending monitoring to a new
// kind of sensor is one row here plus its interpretation in meaning.go.
var classes = map[classKey]Category{
	{DomainSensor, "temperature"}:    CategoryTemperature,
	{DomainSensor, "humidity"}:       CategoryHumidity,
	{DomainSensor, "battery"}:        CategoryBattery,
	{DomainBinarySensor, "battery"}:  CategoryBatteryLow,
	{DomainBinarySensor, "motion"}:   CategoryMotion,
	{DomainBinarySensor, "door"}:     CategoryDoor,
	{DomainBinarySensor, "window"}:   CategoryWindow,
	{DomainBinarySensor, "moisture"}: CategoryLeak,
}

// Classify returns the category for a domain and device class, and false when
// NeptHass does not monitor that kind of sensor.
func Classify(domain, deviceClass string) (Category, bool) {
	c, ok := classes[classKey{domain, deviceClass}]
	return c, ok
}

// Monitored reports whether a domain can contain monitored sensors at all.
func Monitored(domain string) bool {
	return domain == DomainSensor || domain == DomainBinarySensor
}

// Numeric reports whether the category carries a number rather than on/off.
func (c Category) Numeric() bool {
	return c == CategoryTemperature || c == CategoryHumidity || c == CategoryBattery
}

// Label is a short human-readable name.
func (c Category) Label() string {
	switch c {
	case CategoryTemperature:
		return "Temperature"
	case CategoryHumidity:
		return "Humidity"
	case CategoryBattery:
		return "Battery"
	case CategoryBatteryLow:
		return "Battery low"
	case CategoryMotion:
		return "Motion"
	case CategoryDoor:
		return "Door"
	case CategoryWindow:
		return "Window"
	case CategoryLeak:
		return "Water leak"
	default:
		return string(c)
	}
}
