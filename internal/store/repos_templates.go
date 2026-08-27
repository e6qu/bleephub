package store

// LicenseMeta carries the choosealicense.com attributes GitHub returns beyond
// license text. Featured licenses appear in the default /licenses listing.
type LicenseMeta struct {
	Description    string
	Implementation string
	Permissions    []string
	Conditions     []string
	Limitations    []string
	Featured       bool
}

// LicenseMetadata maps each LicenseTemplates key to its metadata.
var LicenseMetadata = map[string]LicenseMeta{
	"mit": {
		Description:    "A short and simple permissive license with conditions only requiring preservation of copyright and license notices. Licensed works, modifications, and larger works may be distributed under different terms and without source code.",
		Implementation: "Create a text file (typically named LICENSE or LICENSE.txt) in the root of your source code and copy the text of the license into the file. Replace [year] with the current year and [fullname] with the name (or names) of the copyright holders.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "private-use"},
		Conditions:     []string{"include-copyright"},
		Limitations:    []string{"liability", "warranty"},
		Featured:       true,
	},
	"apache-2.0": {
		Description:    "A permissive license whose main conditions require preservation of copyright and license notices. Contributors provide an express grant of patent rights. Licensed works, modifications, and larger works may be distributed under different terms and without source code.",
		Implementation: "Create a text file (typically named LICENSE or LICENSE.txt) in the root of your source code and copy the text of the license into the file. Change the copyright notice at the bottom of the text to include your details.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "patent-use", "private-use"},
		Conditions:     []string{"include-copyright", "document-changes"},
		Limitations:    []string{"trademark-use", "liability", "warranty"},
		Featured:       true,
	},
	"gpl-3.0": {
		Description:    "Permissions of this strong copyleft license are conditioned on making available complete source code of licensed works and modifications, which include larger works using a licensed work, under the same license. Copyright and license notices must be preserved. Contributors provide an express grant of patent rights.",
		Implementation: "Create a text file (typically named LICENSE or LICENSE.txt) in the root of your source code and copy the text of the license into the file.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "patent-use", "private-use"},
		Conditions:     []string{"include-copyright", "document-changes", "disclose-source", "same-license"},
		Limitations:    []string{"liability", "warranty"},
		Featured:       true,
	},
	"bsd-2-clause": {
		Description:    "A permissive license that comes in two variants, the BSD 2-Clause and BSD 3-Clause. Both have very minute differences to the MIT license.",
		Implementation: "Create a text file (typically named LICENSE or LICENSE.txt) in the root of your source code and copy the text of the license into the file. Replace [year] with the current year and [fullname] with the name (or names) of the copyright holders.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "private-use"},
		Conditions:     []string{"include-copyright"},
		Limitations:    []string{"liability", "warranty"},
		Featured:       false,
	},
	"bsd-3-clause": {
		Description:    "A permissive license similar to the BSD 2-Clause License, but with a 3rd clause that prohibits others from using the name of the copyright holder or its contributors to promote derived products without written consent.",
		Implementation: "Create a text file (typically named LICENSE or LICENSE.txt) in the root of your source code and copy the text of the license into the file. Replace [year] with the current year and [fullname] with the name (or names) of the copyright holders.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "private-use"},
		Conditions:     []string{"include-copyright"},
		Limitations:    []string{"liability", "warranty"},
		Featured:       false,
	},
	"mpl-2.0": {
		Description:    "Permissions of this weak copyleft license are conditioned on making available source code of licensed files and modifications of those files under the same license (or in certain cases, one of the GNU licenses). Copyright and license notices must be preserved. Contributors provide an express grant of patent rights.",
		Implementation: "Create a text file (typically named LICENSE or LICENSE.txt) in the root of your source code and copy the text of the license into the file.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "patent-use", "private-use"},
		Conditions:     []string{"disclose-source", "include-copyright", "same-license--file"},
		Limitations:    []string{"liability", "trademark-use", "warranty"},
		Featured:       false,
	},
	"unlicense": {
		Description:    "A license with no conditions whatsoever which dedicates works to the public domain. Unlicensed works, modifications, and larger works may be distributed under different terms and without source code.",
		Implementation: "Create a text file (typically named UNLICENSE or LICENSE) in the root of your source code and copy the text of the license into the file.",
		Permissions:    []string{"commercial-use", "modifications", "distribution", "private-use"},
		Conditions:     []string{},
		Limitations:    []string{"liability", "warranty"},
		Featured:       false,
	},
}

// LicenseTemplates maps license keys to full texts. Keys match the SPDX
// identifiers GitHub accepts in the license_template field of repo creation.
var LicenseTemplates = map[string]struct {
	Name   string `json:"-"`
	SpdxID string `json:"-"`
	NodeID string `json:"-"`
	Body   string `json:"-"`
}{
	"mit": {
		Name:   "MIT License",
		SpdxID: "MIT",
		NodeID: "MDc6TGljZW5zZTEz",
		Body: `MIT License

Copyright (c) [year] [fullname]

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`,
	},
	"apache-2.0": {
		Name:   "Apache License 2.0",
		SpdxID: "Apache-2.0",
		NodeID: "MDc6TGljZW5zZTE=",
		Body: `Apache License
Version 2.0, January 2004
http://www.apache.org/licenses/

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
`,
	},
	"gpl-3.0": {
		Name:   "GNU General Public License v3.0",
		SpdxID: "GPL-3.0",
		NodeID: "MDc6TGljZW5zZTE1",
		Body: `GNU GENERAL PUBLIC LICENSE
Version 3, 29 June 2007

Copyright (C) 2007 Free Software Foundation, Inc. <https://fsf.org/>
Everyone is permitted to copy and distribute verbatim copies
of this license document, but changing it is not allowed.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.
`,
	},
	"bsd-2-clause": {
		Name:   "BSD 2-Clause \"Simplified\" License",
		SpdxID: "BSD-2-Clause",
		NodeID: "MDc6TGljZW5zZTQ=",
		Body: `BSD 2-Clause License

Copyright (c) [year], [fullname]
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`,
	},
	"bsd-3-clause": {
		Name:   "BSD 3-Clause \"New\" or \"Revised\" License",
		SpdxID: "BSD-3-Clause",
		NodeID: "MDc6TGljZW5zZTU=",
		Body: `BSD 3-Clause License

Copyright (c) [year], [fullname]
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`,
	},
	"mpl-2.0": {
		Name:   "Mozilla Public License 2.0",
		SpdxID: "MPL-2.0",
		NodeID: "MDc6TGljZW5zZTE2",
		Body: `Mozilla Public License Version 2.0
==================================

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
`,
	},
	"unlicense": {
		Name:   "The Unlicense",
		SpdxID: "Unlicense",
		NodeID: "MDc6TGljZW5zZTE4",
		Body: `This is free and unencumbered software released into the public domain.

Anyone is free to copy, modify, publish, use, compile, sell, or
distribute this software, either in source code form or as a compiled
binary, for any purpose, commercial or non-commercial, and by any
means.

In jurisdictions that recognize copyright laws, the author or authors
of this software dedicate any and all copyright interest in the
software to the public domain. We make this dedication for the benefit
of the public at large and to the detriment of our heirs and
successors. We intend this dedication to be an overt act of
relinquishment in perpetuity of all present and future rights to this
software under copyright law.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
IN NO EVENT SHALL THE AUTHORS BE LIABLE FOR ANY CLAIM, DAMAGES OR
OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE,
ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR
OTHER DEALINGS IN THE SOFTWARE.
`,
	},
}
