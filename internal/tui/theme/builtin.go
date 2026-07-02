package theme

// The built-in themes. Default leans on the sixteen named colors so it
// follows the user's terminal palette instead of fighting it; HighContrast
// drops faint text and color-only distinctions for low-vision and colorblind
// use (state is always carried by weight or reverse video, not hue alone);
// Mono styles with attributes only, for monochrome terminals and pipes that
// strip color but keep attributes.

// Default returns the standard theme.
func Default() *Theme {
	return &Theme{name: "default", styles: map[Role]Style{
		UserPrefix:      {Foreground: "cyan", Bold: true},
		UserText:        {Bold: true},
		Code:            {Foreground: "bright-white"},
		Quote:           {Faint: true, Italic: true},
		Link:            {Foreground: "blue", Underline: true},
		Emphasis:        {Italic: true},
		Strong:          {Bold: true},
		Heading:         {Bold: true, Underline: true},
		ToolName:        {Foreground: "magenta", Bold: true},
		ToolDetail:      {Faint: true},
		ToolOutput:      {Faint: true},
		Admitted:        {Foreground: "green"},
		Rejected:        {Foreground: "red", Bold: true},
		Trust:           {Foreground: "yellow"},
		DiffAdded:       {Foreground: "green"},
		DiffRemoved:     {Foreground: "red"},
		DiffContext:     {Faint: true},
		DiffLocation:    {Foreground: "cyan"},
		Status:          {Faint: true},
		StatusBusy:      {Foreground: "yellow"},
		RecordRecording: {Foreground: "yellow"},
		RecordSealed:    {Foreground: "cyan", Bold: true},
		RecordVerified:  {Foreground: "green", Bold: true},
		RecordFailed:    {Foreground: "red", Bold: true, Reverse: true},
		Success:         {Foreground: "green"},
		Warning:         {Foreground: "yellow"},
		Error:           {Foreground: "red", Bold: true},
		Muted:           {Faint: true},
		Border:          {Faint: true},
		Overlay:         {},
		Selection:       {Reverse: true},
		Placeholder:     {Faint: true},
		PasteChip:       {Foreground: "cyan", Reverse: true},
		QueuedChip:      {Foreground: "yellow", Reverse: true},
	}}
}

// HighContrast returns the accessibility theme: no faint text, and every
// state distinction doubled onto a non-color channel.
func HighContrast() *Theme {
	return &Theme{name: "high-contrast", styles: map[Role]Style{
		UserPrefix:      {Bold: true, Underline: true},
		UserText:        {Bold: true},
		Code:            {Foreground: "bright-white", Background: "black"},
		Quote:           {Italic: true},
		Link:            {Underline: true, Bold: true},
		Emphasis:        {Italic: true},
		Strong:          {Bold: true},
		Heading:         {Bold: true, Underline: true},
		ToolName:        {Bold: true, Underline: true},
		ToolDetail:      {},
		ToolOutput:      {},
		Admitted:        {Bold: true},
		Rejected:        {Bold: true, Reverse: true},
		Trust:           {Bold: true},
		DiffAdded:       {Foreground: "bright-green", Bold: true},
		DiffRemoved:     {Foreground: "bright-red", Bold: true, Underline: true},
		DiffContext:     {},
		DiffLocation:    {Bold: true},
		Status:          {},
		StatusBusy:      {Bold: true},
		RecordRecording: {Bold: true},
		RecordSealed:    {Bold: true, Underline: true},
		RecordVerified:  {Bold: true, Reverse: true},
		RecordFailed:    {Bold: true, Reverse: true, Underline: true},
		Success:         {Bold: true},
		Warning:         {Bold: true, Underline: true},
		Error:           {Bold: true, Reverse: true},
		Muted:           {},
		Border:          {},
		Selection:       {Reverse: true},
		Placeholder:     {Italic: true},
		PasteChip:       {Reverse: true, Bold: true},
		QueuedChip:      {Reverse: true, Underline: true},
	}}
}

// Mono returns the attribute-only theme for monochrome output.
func Mono() *Theme {
	return &Theme{name: "mono", styles: map[Role]Style{
		UserPrefix:   {Bold: true},
		UserText:     {Bold: true},
		Quote:        {Italic: true},
		Link:         {Underline: true},
		Emphasis:     {Italic: true},
		Strong:       {Bold: true},
		Heading:      {Bold: true, Underline: true},
		ToolName:     {Bold: true},
		ToolDetail:   {Faint: true},
		ToolOutput:   {Faint: true},
		Rejected:     {Bold: true, Reverse: true},
		DiffAdded:    {Bold: true},
		DiffRemoved:  {Underline: true},
		DiffContext:  {Faint: true},
		DiffLocation: {Bold: true},
		Status:       {Faint: true},
		RecordFailed: {Reverse: true},
		Error:        {Bold: true, Reverse: true},
		Warning:      {Bold: true},
		Muted:        {Faint: true},
		Border:       {Faint: true},
		Selection:    {Reverse: true},
		Placeholder:  {Faint: true},
		PasteChip:    {Reverse: true},
		QueuedChip:   {Reverse: true},
	}}
}

// Builtin returns the built-in theme with the given name, or nil.
func Builtin(name string) *Theme {
	switch name {
	case "default", "":
		return Default()
	case "high-contrast":
		return HighContrast()
	case "mono":
		return Mono()
	}
	return nil
}
