package hardware

import (
    "fmt"
    "os"
)

func ReadAdcValue(device string, channel int) (int, error) {
    path := fmt.Sprintf("/sys/bus/iio/devices/%s/in_voltage%d_raw", device, channel)
    if _, err := os.Stat(path); os.IsNotExist(err) {
        return -1, fmt.Errorf("ADC sysfs not found: %s", path)
    }
    
    data, err := os.ReadFile(path)
    if err != nil {
        return -1, fmt.Errorf("failed reading %s: %w", path, err)
    }
    
    var value int
    _, err = fmt.Sscanf(string(data), "%d", &value)
    if err != nil {
        return -1, fmt.Errorf("failed parsing ADC value: %w", err)
    }
    
    return value, nil
}

func InRange(v, min, max int) bool {
    return v >= min && v <= max
}
