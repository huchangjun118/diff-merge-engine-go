package patch

// WithDefaults returns a copy of the options with zero values replaced by
// sensible defaults.
func (o ApplyOptions) WithDefaults() ApplyOptions {
	if o.Fuzz <= 0 {
		o.Fuzz = 2
	}
	if o.MaxOffset <= 0 {
		o.MaxOffset = 100
	}
	return o
}
